package main

import (
	"context"
	"io"

	"github.com/nathants/go-libsodium"
)

type pushPlaintext struct {
	ctx context.Context
	io.Reader
}

func (r pushPlaintext) Read(p []byte) (int, error) {
	if err := context.Cause(r.ctx); err != nil {
		return 0, err
	}
	return r.Reader.Read(p)
}

type pushCiphertext struct {
	ctx context.Context
	io.Writer
}

func (w pushCiphertext) Write(p []byte) (int, error) {
	if err := context.Cause(w.ctx); err != nil {
		return 0, err
	}
	return w.Writer.Write(p)
}

// The stream codec remains in go-libsodium. Check cancellation at each chunk and
// recipient write; closing the owned streams also interrupts blocked pipe I/O.
func encryptPushBundle(ctx context.Context, recipients [][]byte, plain io.ReadCloser, cipher io.WriteCloser) error {
	if err := context.Cause(ctx); err != nil {
		return err
	}
	done := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		defer close(done)
		_ = plain.Close()
		_ = cipher.Close()
	})
	defer func() {
		if !stop() {
			<-done
		}
	}()
	err := libsodium.StreamEncryptRecipients(recipients, pushPlaintext{ctx, plain}, pushCiphertext{ctx, cipher})
	if cause := context.Cause(ctx); cause != nil {
		return cause
	}
	return err
}
