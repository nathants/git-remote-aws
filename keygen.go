package main

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"

	"github.com/nathants/go-libsodium"
	"github.com/nathants/go-libsodium/keysource"
	"golang.org/x/sys/unix"
)

func keygen(args []string, output io.Writer) error {
	flags := flag.NewFlagSet("keygen", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	publicPath := flags.String("public-key-file", "", "personal public chain file")
	secretPath := flags.String("secret-key-file", "", "personal private chain file")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			_, writeErr := fmt.Fprintln(output, "usage: git-remote-aws --keygen [--public-key-file PATH --secret-key-file PATH]\nBoth absent creates personal files; both present extends their matching chains.\nWithout paths, emit a fresh pair for manual secret-manager storage.")
			return writeErr
		}
		return err
	}
	if flags.NArg() != 0 || (flags.NFlag() != 0 && (*publicPath == "" || *secretPath == "")) {
		return fmt.Errorf("keygen requires both nonempty --public-key-file and --secret-key-file, or neither flag")
	}
	if *publicPath == "" {
		pk, sk, err := libsodium.BoxKeypair()
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(output, "export GIT_REMOTE_AWS_PUBLICKEY=%s\nexport GIT_REMOTE_AWS_SECRETKEY=%s\n", hex.EncodeToString(pk), hex.EncodeToString(sk))
		return err
	}
	return rotateKeyFiles(*publicPath, *secretPath)
}

// Keys are personal files, not repository .publickeys lists. Each containing
// directory must be exclusively controlled by the operator. Directory flocks
// serialize cooperating keygen invocations, including cross-directory pairs.
func rotateKeyFiles(publicPath, secretPath string) error {
	publicPath, err := filepath.Abs(publicPath)
	if err != nil {
		return err
	}
	secretPath, err = filepath.Abs(secretPath)
	if err != nil {
		return err
	}
	if publicPath == secretPath {
		return fmt.Errorf("public and private files must be distinct")
	}
	dirs := []string{filepath.Dir(publicPath), filepath.Dir(secretPath)}
	sort.Strings(dirs)
	var locks []*os.File
	defer func() {
		for _, f := range locks {
			_ = f.Close()
		}
	}()
	for i, dir := range dirs {
		if i > 0 && dir == dirs[i-1] {
			continue
		}
		fd, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		if err != nil {
			return err
		}
		f := os.NewFile(uintptr(fd), dir)
		locks = append(locks, f)
		if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
			return fmt.Errorf("key directory is busy: %w", err)
		}
	}
	pinfo, perr := os.Lstat(publicPath)
	sinfo, serr := os.Lstat(secretPath)
	fresh := errors.Is(perr, os.ErrNotExist) && errors.Is(serr, os.ErrNotExist)
	var public, secret libsodium.KeyChains
	if !fresh {
		if perr != nil || serr != nil {
			return fmt.Errorf("both key files must exist, or both must be absent; preserve existing files and reconcile explicitly")
		}
		if os.SameFile(pinfo, sinfo) {
			return fmt.Errorf("public and private files refer to the same inode")
		}
		public, err = keysource.ReadFile(publicPath, false)
		if err != nil {
			return err
		}
		secret, err = keysource.ReadFile(secretPath, true)
		if err != nil {
			return err
		}
		if len(public) != 1 || len(secret) != 1 {
			return fmt.Errorf("existing personal files must each contain exactly one chain")
		}
	}
	public, secret, err = libsodium.RotateKeyChain(public, secret)
	if err != nil {
		return err
	}
	pbytes, err := public.MarshalText()
	if err != nil {
		return err
	}
	sbytes, err := secret.MarshalText()
	if err != nil {
		return err
	}
	if err := publishKeyFile(secretPath, sbytes, fresh); err != nil {
		return fmt.Errorf("private key publication failed; preserve both files and inspect before retry: %w", err)
	}
	if err := publishKeyFile(publicPath, pbytes, fresh); err != nil {
		return fmt.Errorf("private chain was extended but public publication failed; preserve both files and reconcile before retry: %w", err)
	}
	return nil
}

func publishKeyFile(path string, data []byte, fresh bool) error {
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return err
	}
	temp := ".keygen-" + hex.EncodeToString(id[:])
	f, err := root.OpenFile(temp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer func() { _ = root.Remove(temp) }()
	_, writeErr := f.Write(data)
	syncErr := f.Sync()
	closeErr := f.Close()
	if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
		return err
	}
	if fresh {
		// Never replace a file that appeared after the initial absence check.
		if err := root.Link(temp, filepath.Base(path)); err != nil {
			return err
		}
		if err := root.Remove(temp); err != nil {
			return err
		}
	} else if err := root.Rename(temp, filepath.Base(path)); err != nil {
		return err
	}
	dir, err := root.Open(".")
	if err != nil {
		return err
	}
	syncErr = dir.Sync()
	closeErr = dir.Close()
	return errors.Join(syncErr, closeErr)
}
