package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

type encryptionReader struct {
	io.ReadCloser
	started chan struct{}
	once    sync.Once
}

func (r *encryptionReader) Read(p []byte) (int, error) {
	r.once.Do(func() { close(r.started) })
	return r.ReadCloser.Read(p)
}

type encryptionWriter struct{ io.Writer }

func (encryptionWriter) Close() error { return nil }

func TestEncryptionCancellationInterruptsBlockedRead(t *testing.T) {
	setupEphemeralKeys(t)
	r, w := io.Pipe()
	defer func() { _ = r.Close(); _ = w.Close() }()
	reader := &encryptionReader{ReadCloser: r, started: make(chan struct{})}
	ctx, cancel := context.WithCancelCause(t.Context())
	defer cancel(nil)
	lost := errors.New("lease lost during encryption")
	var ciphertext bytes.Buffer
	done := make(chan error, 1)
	go func() { done <- encryptPushBundle(ctx, publicKey(), reader, encryptionWriter{&ciphertext}) }()
	select {
	case <-reader.started:
	case <-time.After(time.Second):
		t.Fatal("encryption did not read input")
	}
	cancel(lost)
	select {
	case err := <-done:
		if !errors.Is(err, lost) {
			t.Fatalf("encryption lost cancellation cause: %v", err)
		}
	case <-time.After(time.Second):
		_ = w.Close()
		<-done
		t.Fatal("encryption kept reading after lease loss")
	}
}

func TestEncryptionCanceledBeforeStartAndRoundTrip(t *testing.T) {
	setupEphemeralKeys(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	var ciphertext bytes.Buffer
	if err := encryptPushBundle(ctx, publicKey(), io.NopCloser(strings.NewReader("plaintext")), encryptionWriter{&ciphertext}); !errors.Is(err, context.Canceled) || ciphertext.Len() != 0 {
		t.Fatalf("canceled encryption wrote data: %v (%d bytes)", err, ciphertext.Len())
	}
	if err := encryptPushBundle(t.Context(), publicKey(), io.NopCloser(strings.NewReader("plaintext")), encryptionWriter{&ciphertext}); err != nil {
		t.Fatal(err)
	}
	var plaintext bytes.Buffer
	ring := secretKey(t.Context(), "")
	if err := ring.Decrypt(&ciphertext, &plaintext); err != nil || plaintext.String() != "plaintext" {
		t.Fatalf("encrypted format changed: %v", err)
	}
}

type encryptionBlockedWriter struct {
	io.WriteCloser
	started chan struct{}
	once    sync.Once
}

func (w *encryptionBlockedWriter) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.started) })
	return w.WriteCloser.Write(p)
}

func TestEncryptionCancellationInterruptsBlockedWrite(t *testing.T) {
	setupEphemeralKeys(t)
	r, w := io.Pipe()
	defer func() { _ = r.Close(); _ = w.Close() }()
	writer := &encryptionBlockedWriter{WriteCloser: w, started: make(chan struct{})}
	ctx, cancel := context.WithCancelCause(t.Context())
	defer cancel(nil)
	lost := errors.New("lease lost during encryption write")
	done := make(chan error, 1)
	go func() {
		done <- encryptPushBundle(ctx, publicKey(), io.NopCloser(strings.NewReader("plaintext")), writer)
	}()
	select {
	case <-writer.started:
	case <-time.After(time.Second):
		t.Fatal("encryption did not write output")
	}
	cancel(lost)
	select {
	case err := <-done:
		if !errors.Is(err, lost) {
			t.Fatalf("encryption lost cancellation cause: %v", err)
		}
	case <-time.After(time.Second):
		_ = r.Close()
		<-done
		t.Fatal("encryption kept writing after lease loss")
	}
}

func TestEncryptionFetchCancellationAndRoundTrip(t *testing.T) {
	setupEphemeralKeys(t)
	ring := secretKey(t.Context(), "")
	var ciphertext bytes.Buffer
	if err := encryptPushBundle(t.Context(), publicKey(), io.NopCloser(strings.NewReader("plaintext")), encryptionWriter{&ciphertext}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	var plaintext bytes.Buffer
	if err := decryptFetchBundle(ctx, ring, io.NopCloser(bytes.NewReader(ciphertext.Bytes())), encryptionWriter{&plaintext}); !errors.Is(err, context.Canceled) || plaintext.Len() != 0 {
		t.Fatalf("canceled decryption wrote data: %v (%d bytes)", err, plaintext.Len())
	}
	if err := decryptFetchBundle(t.Context(), ring, io.NopCloser(&ciphertext), encryptionWriter{&plaintext}); err != nil || plaintext.String() != "plaintext" {
		t.Fatalf("fetch decryption changed: %v", err)
	}
}
