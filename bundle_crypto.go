package main

import (
	"context"
	"io"

	"github.com/nathants/go-libsodium"
)

func encryptPushBundle(ctx context.Context, recipients [][]byte, plain io.ReadCloser, cipher io.WriteCloser) error {
	return streamBundle(ctx, plain, cipher, func(r io.Reader, w io.Writer) error {
		return libsodium.StreamEncryptRecipients(recipients, r, w)
	})
}

func decryptFetchBundle(ctx context.Context, ring *libsodium.Keyring, cipher io.ReadCloser, plain io.WriteCloser) error {
	return streamBundle(ctx, cipher, plain, ring.Decrypt)
}

type bundleReader struct {
	ctx context.Context
	io.Reader
}

func (r bundleReader) Read(p []byte) (int, error) {
	if err := context.Cause(r.ctx); err != nil {
		return 0, err
	}
	return r.Reader.Read(p)
}

type bundleWriter struct {
	ctx context.Context
	io.Writer
}

func (w bundleWriter) Write(p []byte) (int, error) {
	if err := context.Cause(w.ctx); err != nil {
		return 0, err
	}
	return w.Writer.Write(p)
}

// Codecs remain in go-libsodium. Check cancellation between chunks and close
// the owned streams to interrupt blocked I/O during either encryption or decryption.
func streamBundle(ctx context.Context, source io.ReadCloser, target io.WriteCloser, codec func(io.Reader, io.Writer) error) error {
	if err := context.Cause(ctx); err != nil {
		return err
	}
	done := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		defer close(done)
		_ = source.Close()
		_ = target.Close()
	})
	defer func() {
		if !stop() {
			<-done
		}
	}()
	err := codec(bundleReader{ctx, source}, bundleWriter{ctx, target})
	if cause := context.Cause(ctx); cause != nil {
		return cause
	}
	return err
}
