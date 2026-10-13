package engine

import (
	"context"
	"encoding/base64"
	"fmt"

	"github.com/frgmt0/depper/internal/shell"
)

// uploadModes are the file modes Upload accepts.
var uploadModes = map[string]bool{"600": true, "644": true, "700": true}

// UploadFile is one file for UploadFiles.
type UploadFile struct {
	Contents []byte
	// Destination is an absolute plain remote path.
	Destination string
	// Mode is "600", "644", or "700".
	Mode string
}

// uploadLines write one file atomically: a private temporary file next to
// the destination (umask 077, mktemp), then chmod and rename. The content
// travels base64-encoded on ssh's stdin, never in argv, and a symlinked
// destination is refused.
func uploadLines(f UploadFile) ([]string, error) {
	if !uploadModes[f.Mode] {
		return nil, fmt.Errorf("invalid upload mode %q", f.Mode)
	}
	if !shell.IsSafePath(f.Destination) {
		return nil, fmt.Errorf("refusing to upload to remote path %q (only %s are supported)", f.Destination, shell.SafePathChars)
	}
	encoded := base64.StdEncoding.EncodeToString(f.Contents)
	return []string{
		"destination=" + shell.Quote(f.Destination),
		refuseSymlink(`"$destination"`, "upload destination"),
		`parent=$(dirname "$destination")`,
		`test -d "$parent"`,
		`temporary=$(mktemp "$parent/.depper-upload.XXXXXX")`,
		`trap 'rm -f "$temporary"' EXIT HUP INT TERM`,
		"printf %s " + shell.Quote(encoded) + ` | base64 -d > "$temporary"`,
		"chmod " + f.Mode + ` "$temporary"`,
		`mv -Tf "$temporary" "$destination"`,
		"trap - EXIT HUP INT TERM",
	}, nil
}

func uploadScript(files ...UploadFile) (string, error) {
	lines := []string{"set +xv", "set -eu", "umask 077"}
	for _, f := range files {
		more, err := uploadLines(f)
		if err != nil {
			return "", err
		}
		lines = append(lines, more...)
	}
	return shell.Script(lines...), nil
}

// UploadFiles writes several files in one round trip, each atomically and
// privately (see Upload).
func (e *Engine) UploadFiles(ctx context.Context, files ...UploadFile) error {
	if len(files) == 0 {
		return nil
	}
	script, err := uploadScript(files...)
	if err != nil {
		return err
	}
	return e.Remote.Run(ctx, script)
}

// Upload writes contents to the remote file destination (an absolute plain
// path) with the given mode ("600", "644" or "700"), atomically and
// privately.
func (e *Engine) Upload(ctx context.Context, contents []byte, destination, mode string) error {
	return e.UploadFiles(ctx, UploadFile{Contents: contents, Destination: destination, Mode: mode})
}
