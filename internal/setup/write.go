package setup

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/linchange/linchange/config"
)

// fileHeader is written above the generated document.
//
// The wizard's output is a plain configuration file, not a proprietary blob:
// an operator who has to change one setting at 3am must be able to edit it,
// and needs to be told where the documentation lives and which values are
// secrets.
const fileHeader = `# linchange configuration, written by "linchange setup".
#
# Every setting is documented in the example file that ships with the package
# at /etc/linchange/linchange.example.yaml. The values below are the ones the
# wizard chose; anything not asked about keeps its built-in default.
#
# The secrets in this file (auth.jwt.secret, auth.encryption_key and
# mail.dkim_private_key_encryption_key) were generated for this installation.
# Changing them has consequences: a new jwt.secret signs every user out, and a
# new encryption_key makes already-enrolled second factors unreadable.
#
# After editing: systemctl restart linchange
`

// WriteFile writes cfg to path and returns the path of the backup when an
// existing file was replaced.
//
// Three properties matter here. The file is written atomically, so an
// interrupted run cannot leave a half-written configuration for the service to
// trip over. It is written 0600, because the database password and the signing
// keys live in it. And the new content is loaded back before it is put in
// place, so a document that this binary would refuse to read is never the one
// on disk.
func WriteFile(path string, cfg *config.Config) (string, error) {
	body, err := marshal(cfg)
	if err != nil {
		return "", err
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("creating %s: %w", dir, err)
	}

	tmp, err := os.CreateTemp(dir, ".linchange-setup-*")
	if err != nil {
		return "", fmt.Errorf("creating a temporary file in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if err := writeAndCheck(tmp, tmpName, body); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("closing %s: %w", tmpName, err)
	}

	backup, err := backupExisting(path)
	if err != nil {
		return "", err
	}

	if err := os.Rename(tmpName, path); err != nil {
		return backup, fmt.Errorf("replacing %s: %w", path, err)
	}
	// The rename preserves the temporary file's mode, which is 0600 from
	// CreateTemp, but stating it costs nothing and catches a future change.
	if err := os.Chmod(path, 0o600); err != nil {
		return backup, fmt.Errorf("restricting %s: %w", path, err)
	}
	return backup, nil
}

// writeAndCheck writes the document, syncs it and proves it loads. Checking on
// the temporary file means the check and the write cannot disagree.
func writeAndCheck(f *os.File, tmpName string, body []byte) error {
	if _, err := f.Write(body); err != nil {
		return fmt.Errorf("writing the configuration: %w", err)
	}
	if err := f.Chmod(0o600); err != nil {
		return fmt.Errorf("restricting the configuration: %w", err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("syncing the configuration: %w", err)
	}
	if _, err := config.Load(tmpName); err != nil {
		return fmt.Errorf("the generated configuration does not load: %w", err)
	}
	return nil
}

// backupExisting copies an existing configuration aside, returning its path.
// A copy rather than a rename, because the service may be reading the file
// right now and a rename would pull it out from under an open handle.
func backupExisting(path string) (string, error) {
	src, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("reading the existing %s: %w", path, err)
	}
	defer src.Close()

	backup := fmt.Sprintf("%s.bak-%s", path, time.Now().Format("20060102-150405"))
	dst, err := os.OpenFile(backup, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", fmt.Errorf("creating the backup %s: %w", backup, err)
	}
	if _, err := io.Copy(dst, src); err != nil {
		dst.Close()
		return "", fmt.Errorf("writing the backup %s: %w", backup, err)
	}
	if err := dst.Sync(); err != nil {
		dst.Close()
		return "", fmt.Errorf("syncing the backup %s: %w", backup, err)
	}
	if err := dst.Close(); err != nil {
		return "", fmt.Errorf("closing the backup %s: %w", backup, err)
	}
	return backup, nil
}

// marshal renders the configuration with the wizard's header. The header is
// bytes rather than YAML comments on the struct, because the encoder owns the
// document's layout and a comment carried in a struct field would be lost on
// the first edit.
func marshal(cfg *config.Config) ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteString(fileHeader)

	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(cfg); err != nil {
		return nil, fmt.Errorf("encoding the configuration: %w", err)
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("encoding the configuration: %w", err)
	}
	return buf.Bytes(), nil
}
