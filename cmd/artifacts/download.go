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
	"github.com/mattn/go-isatty"
)

type DownloadCmd struct {
	ArtifactID string `arg:"" help:"Artifact UUID to download"`
}

func (c *DownloadCmd) Help() string {
	return `
Use this command to download a specific artifact.

Examples:
  # Download an artifact by UUID
  $ bk artifacts download 0191727d-b5ce-4576-b37d-477ae0ca830c
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

	downloadDir, downloadErr := download(ctx, f, c.ArtifactID)
	if downloadErr != nil {
		return downloadErr
	}

	fmt.Printf("Downloaded artifact to: %s\n", downloadDir)
	return nil
}

func download(ctx context.Context, f *factory.Factory, artifactID string) (string, error) {
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

	apiResp, apiErr := http.Get(resp.Artifact.DownloadURL)
	if apiErr != nil {
		return "", apiErr
	}
	defer apiResp.Body.Close()

	if apiResp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("failed to fetch the requested artifact (status %s)", apiResp.Status)
	}

	reader := io.Reader(apiResp.Body)
	var pw *progressWriter
	if apiResp.ContentLength > 0 && !f.Quiet && isatty.IsTerminal(os.Stderr.Fd()) {
		pw = &progressWriter{total: apiResp.ContentLength, label: filename}
		pw.print(false)
		reader = io.TeeReader(apiResp.Body, pw)
	}

	_, err = io.Copy(out, reader)
	if pw != nil {
		pw.finish()
	}
	if err != nil {
		return "", err
	}

	return directory, nil
}

type progressWriter struct {
	total     int64
	written   int64
	lastPrint time.Time
	lastLen   int
	label     string
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
