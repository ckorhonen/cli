package artifacts

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/alecthomas/kong"
	"github.com/buildkite/cli/v3/internal/artifact"
	"github.com/buildkite/cli/v3/internal/cli"
	bkGraphQL "github.com/buildkite/cli/v3/internal/graphql"
	bkIO "github.com/buildkite/cli/v3/internal/io"
	"github.com/buildkite/cli/v3/pkg/cmd/factory"
	"github.com/buildkite/cli/v3/pkg/cmd/validation"
	buildkite "github.com/buildkite/go-buildkite/v4"
	"github.com/mattn/go-isatty"
)

const downloadTimeout = 10 * time.Minute

type DownloadCmd struct {
	ArtifactID string `arg:"" optional:"" help:"Artifact ID (UUID) to download"`
	Pipeline   string `help:"This can be provided as a {pipeline-slug} or {org-slug}/{pipeline-slug} for multi-org setups." short:"p"`
	JobID      string `help:"Download all artifacts for a job UUID"`
}

func (c *DownloadCmd) Help() string {
	return `
Use this command to download artifacts.

Examples:
	# Download an artifact by UUID
	$ bk artifacts download 0191727d-b5ce-4576-b37d-477ae0ca830c

	# Download all artifacts for a job on the latest build for the current branch
	$ bk artifacts download --job-id 0193903e-ecd9-4c51-9156-0738da987e87 -p my-pipeline
`
}

func (c *DownloadCmd) Run(kongCtx *kong.Context, globals cli.GlobalFlags) error {
	f, err := factory.New()
	if err != nil {
		return err
	}

	f.SkipConfirm = globals.SkipConfirmation()
	f.NoInput = globals.DisableInput()
	f.Quiet = globals.IsQuiet()

	if err := validation.ValidateConfiguration(f.Config, kongCtx.Command()); err != nil {
		return err
	}

	ctx := context.Background()

	artifactID := c.ArtifactID
	if artifactID == "" && c.JobID == "" {
		return fmt.Errorf("an artifact UUID or --job-id is required")
	}
	if artifactID != "" && c.JobID != "" {
		return fmt.Errorf("choose only one source: artifact UUID or --job-id")
	}
	if c.JobID == "" && c.Pipeline != "" {
		// something, something verbose flag...
		// will remove this in future, probably will just comment this out for now
		fmt.Fprintf(os.Stdout, "Info: ignoring --pipeline flag as it can only be used when targeting a job with --job-id\n")
	}

	if c.JobID != "" {
		return downloadJobArtifacts(ctx, f, c)
	}

	downloadDir, downloadErr := downloadArtifact(ctx, f, artifactID)
	if downloadErr != nil {
		return downloadErr
	}

	fmt.Printf("Downloaded artifact to: %s\n", downloadDir)
	return nil
}

func downloadArtifact(ctx context.Context, f *factory.Factory, artifactID string) (string, error) {
	resp, err := bkGraphQL.GetArtifacts(ctx, f.GraphQLClient, artifactID)
	if err != nil {
		return "", err
	}

	if resp == nil || resp.Artifact == nil {
		return "", fmt.Errorf("no artifact found with ID: %s", artifactID)
	}

	directory := fmt.Sprintf("artifact-%s", artifactID)
	if err := os.MkdirAll(directory, os.ModePerm); err != nil {
		return "", err
	}

	filename := filepath.Base(resp.Artifact.Path)
	out, fileErr := os.Create(filepath.Join(directory, filename))
	if fileErr != nil {
		return "", fileErr
	}
	defer out.Close()

	if err := downloadToFile(ctx, downloadHTTPClient(), resp.Artifact.DownloadURL, filename, filename, out, f.Quiet); err != nil {
		return "", err
	}

	return directory, nil
}

func downloadJobArtifacts(ctx context.Context, f *factory.Factory, cmd *DownloadCmd) error {
	org := f.Config.OrganizationSlug()
	pipelineSlug := ""
	buildNumber := ""

	jobCtx, err := resolveJobContext(ctx, f, cmd.JobID)
	if err != nil {
		return err
	}
	pipelineSlug = jobCtx.PipelineSlug
	if pipelineSlug == "" {
		return fmt.Errorf("no pipeline found for job: %s", cmd.JobID)
	}

	if cmd.Pipeline != "" {
		pFlag := cmd.Pipeline
		if strings.Contains(pFlag, "/") {
			parts := strings.Split(pFlag, "/")
			pFlag = parts[len(parts)-1]
		}
		if pipelineSlug != pFlag {
			return fmt.Errorf("job belongs to pipeline %s, but --pipeline was %s", pipelineSlug, cmd.Pipeline)
		}
	}

	buildNumber = fmt.Sprint(jobCtx.BuildNumber)

	var (
		artifacts []buildkite.Artifact
		listErr   error
	)
	if err := bkIO.SpinWhile(f, "Loading job artifacts", func() {
		var resp []buildkite.Artifact
		resp, _, listErr = f.RestAPIClient.Artifacts.ListByJob(ctx, org, pipelineSlug, buildNumber, cmd.JobID, nil)
		if listErr == nil {
			artifacts = resp
		}
	}); err != nil {
		return err
	}
	if listErr != nil {
		return listErr
	}

	if len(artifacts) == 0 {
		return fmt.Errorf("no artifacts found for job: %s", cmd.JobID)
	}

	baseDir := fmt.Sprintf("job-%s", cmd.JobID)
	if err := os.MkdirAll(baseDir, os.ModePerm); err != nil {
		return err
	}

	client := downloadHTTPClient()
	for _, a := range artifacts {
		artifactPath := a.Path
		if artifactPath == "" {
			artifactPath = a.Filename
		}
		dir := filepath.Join(baseDir, filepath.Dir(artifactPath))
		if err := os.MkdirAll(dir, os.ModePerm); err != nil {
			return err
		}

		filename := filepath.Base(artifactPath)
		if filename == "." || filename == "" {
			filename = a.ID
		}
		targetPath := filepath.Join(dir, filename)

		file, err := os.Create(targetPath)
		if err != nil {
			return err
		}

		req, reqErr := f.RestAPIClient.NewRequest(ctx, http.MethodGet, a.DownloadURL, nil)
		if reqErr != nil {
			file.Close()
			return reqErr
		}
		if err := downloadWithRequest(client, req, filename, artifactPath, file, f.Quiet); err != nil {
			file.Close()
			return err
		}
		_ = file.Close()
		fmt.Printf("Downloaded artifact to: %s\n", targetPath)
	}

	return nil
}

type progressWriter struct {
	total     int64
	written   int64
	lastPrint time.Time
	lastLen   int
	label     string
}

func downloadToFile(ctx context.Context, client *http.Client, url, label, artifactPath string, out io.Writer, quiet bool) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	return downloadWithRequest(client, req, label, artifactPath, out, quiet)
}

func downloadWithRequest(client *http.Client, req *http.Request, label, artifactPath string, out io.Writer, quiet bool) error {
	resp, err := client.Do(req)
	if err != nil {
		return err
	}

	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("failed to fetch the requested artifact (status %s)", resp.Status)
	}

	reader := io.Reader(resp.Body)
	var pw *progressWriter
	if resp.ContentLength > 0 && !quiet && isatty.IsTerminal(os.Stderr.Fd()) {
		displayLabel := label
		if artifactPath != "" && artifactPath != label {
			displayLabel = fmt.Sprintf("%s (%s)", label, artifactPath)
		}
		pw = &progressWriter{total: resp.ContentLength, label: displayLabel}
		pw.print(false)
		reader = io.TeeReader(resp.Body, pw)
	}

	_, err = io.Copy(out, reader)
	if pw != nil {
		pw.finish()
	}
	if err != nil {
		return err
	}

	return nil
}

func downloadHTTPClient() *http.Client {
	return &http.Client{Timeout: downloadTimeout}
}

type jobContext struct {
	PipelineSlug string
	BuildNumber  int
}

func resolveJobContext(ctx context.Context, f *factory.Factory, jobID string) (*jobContext, error) {
	resp, err := bkGraphQL.GetJobArtifacts(ctx, f.GraphQLClient, jobID)
	if err != nil {
		return nil, err
	}
	if resp.Job == nil || *resp.Job == nil {
		return nil, fmt.Errorf("no job found with ID: %s", jobID)
	}

	job := *resp.Job

	cmdJob, ok := job.(*bkGraphQL.GetJobArtifactsJobJobTypeCommand)
	if !ok || cmdJob == nil {
		return nil, fmt.Errorf("job %s is not a command job with artifacts", jobID)
	}
	if cmdJob.Build == nil || cmdJob.Pipeline == nil {
		return nil, fmt.Errorf("no build or pipeline found for job: %s", jobID)
	}

	pipelineSlug := cmdJob.Pipeline.Slug
	if pipelineSlug == "" {
		pipelineSlug = cmdJob.Pipeline.Name
	}

	return &jobContext{
		PipelineSlug: pipelineSlug,
		BuildNumber:  cmdJob.Build.Number,
	}, nil
}

func (p *progressWriter) Write(b []byte) (int, error) {
	n := len(b)
	p.written += int64(n)
	p.print(false)
	return n, nil
}

func (p *progressWriter) finish() {
	p.print(true)
}

func (p *progressWriter) print(force bool) {
	now := time.Now()
	if !force && !p.lastPrint.IsZero() && now.Sub(p.lastPrint) < 200*time.Millisecond {
		return
	}
	totalForBar := p.total
	if totalForBar <= 0 {
		totalForBar = 1
	}
	percent := int(min64(p.written*100/totalForBar, 100))
	bar := bkIO.ProgressBar(int(p.written), int(totalForBar), 30)
	written := artifact.FormatBytes(p.written)
	total := artifact.FormatBytes(p.total)
	line := fmt.Sprintf("Downloading %s %s %3d%% (%s/%s)", p.label, bar, percent, written, total)
	pad := p.lastLen - len(line)
	if pad < 0 {
		pad = 0
	}
	fmt.Fprintf(os.Stderr, "\r%s%s", line, strings.Repeat(" ", pad))
	p.lastLen = len(line)
	if force {
		fmt.Fprint(os.Stderr, "\n")
	}
	p.lastPrint = now
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}
