package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/Automaat/cache-buster/internal/config"
	"github.com/Automaat/cache-buster/pkg/size"
	"github.com/kballard/go-shellquote"
)

// DockerProvider cleans Docker caches when daemon is available.
type DockerProvider struct {
	*BaseProvider
	cleanCmd string
}

// NewDockerProvider creates a Docker provider with availability checking.
func NewDockerProvider(name string, cfg config.Provider) (*DockerProvider, error) {
	base, err := NewBaseProvider(name, cfg)
	if err != nil {
		return nil, err
	}

	return &DockerProvider{
		BaseProvider: base,
		cleanCmd:     cfg.CleanCmd,
	}, nil
}

// dockerDFRow is one line of docker system df --format '{{json .}}' output.
type dockerDFRow struct {
	Size string `json:"Size"`
}

// CurrentSize returns actual Docker data usage from docker system df.
// Falls back to path-based size on any error (daemon stopped, timeouts,
// permission issues) so status always reports something useful.
func (p *DockerProvider) CurrentSize() (int64, error) {
	if b, err := p.dockerDataSize(); err == nil {
		return b, nil
	}
	return p.BaseProvider.CurrentSize()
}

// DiskImageSize returns the path-based filesystem size of the configured Docker paths.
func (p *DockerProvider) DiskImageSize() (int64, error) {
	return p.BaseProvider.CurrentSize()
}

func (p *DockerProvider) dockerDataSize() (int64, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "docker", "system", "df", "--format", "{{json .}}")
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg != "" {
			return 0, fmt.Errorf("docker system df: %w: %s", err, msg)
		}
		return 0, fmt.Errorf("docker system df: %w", err)
	}

	var total int64
	var firstErr error
	var rowsParsed int

	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		var row dockerDFRow
		if jsonErr := json.Unmarshal([]byte(line), &row); jsonErr != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("unmarshal docker df line %q: %w", line, jsonErr)
			}
			continue
		}
		b, parseErr := size.ParseSize(row.Size)
		if parseErr != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("parse docker size %q: %w", row.Size, parseErr)
			}
			continue
		}
		total += b
		rowsParsed++
	}

	if rowsParsed == 0 {
		if firstErr != nil {
			return 0, firstErr
		}
		return 0, fmt.Errorf("docker system df: no parsable output")
	}

	// Some rows parsed successfully; treat partial parse errors as non-fatal.
	return total, nil
}

// Available implements Provider.
func (p *DockerProvider) Available() bool {
	if _, err := exec.LookPath("docker"); err != nil {
		return false
	}

	cmd := exec.Command("docker", "ps", "--quiet")
	return cmd.Run() == nil
}

// Clean implements Provider.
func (p *DockerProvider) Clean(ctx context.Context, opts CleanOptions) (CleanResult, error) {
	if !p.Available() {
		return CleanResult{
			Output: "docker not available",
		}, nil
	}

	if opts.Mode == CleanModeSmart {
		return p.smartClean(ctx, opts)
	}
	return p.fullClean(ctx, opts)
}

func (p *DockerProvider) smartClean(ctx context.Context, opts CleanOptions) (CleanResult, error) {
	hours := int64(p.maxAge.Hours())
	if hours < 1 {
		hours = 1
	}
	filterArg := fmt.Sprintf("until=%dh", hours)
	smartCmd := fmt.Sprintf("docker system prune -af --filter %s", filterArg)

	if opts.DryRun {
		return CleanResult{
			Output: "would run: " + smartCmd,
		}, nil
	}

	sizeBefore, _ := p.CurrentSize()

	cmd := exec.CommandContext(ctx, "docker", "system", "prune", "-af", "--filter", filterArg)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	output := strings.TrimSpace(stdout.String() + stderr.String())

	if err != nil {
		return CleanResult{Output: output}, err
	}

	sizeAfter, _ := p.CurrentSize()
	bytesCleaned := sizeBefore - sizeAfter
	if bytesCleaned < 0 {
		fmt.Fprintf(os.Stderr, "warning: %s cache size increased during clean\n", p.name)
		bytesCleaned = 0
	}

	return CleanResult{
		BytesCleaned: bytesCleaned,
		Output:       output,
	}, nil
}

func (p *DockerProvider) fullClean(ctx context.Context, opts CleanOptions) (CleanResult, error) {
	if opts.DryRun {
		return CleanResult{
			Output: "would run: " + p.cleanCmd,
		}, nil
	}

	sizeBefore, _ := p.CurrentSize()

	parts, err := shellquote.Split(p.cleanCmd)
	if err != nil {
		return CleanResult{}, fmt.Errorf("invalid command: %w", err)
	}
	if len(parts) == 0 {
		return CleanResult{}, nil
	}

	cmd := exec.CommandContext(ctx, parts[0], parts[1:]...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err = cmd.Run()
	output := strings.TrimSpace(stdout.String() + stderr.String())

	if err != nil {
		return CleanResult{Output: output}, err
	}

	sizeAfter, _ := p.CurrentSize()
	bytesCleaned := sizeBefore - sizeAfter
	if bytesCleaned < 0 {
		fmt.Fprintf(os.Stderr, "warning: %s cache size increased during clean\n", p.name)
		bytesCleaned = 0
	}

	return CleanResult{
		BytesCleaned: bytesCleaned,
		Output:       output,
	}, nil
}
