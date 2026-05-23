package queue

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestQueueAcquireRelease(t *testing.T) {
	m := New(5000, 100, 2)
	r1, err := m.Acquire(context.Background(), "s")
	if err != nil {
		t.Fatal(err)
	}
	r2, err := m.Acquire(context.Background(), "s")
	if err != nil {
		t.Fatal(err)
	}
	// third call should block until a slot is released.
	done := make(chan error, 1)
	go func() {
		_, e := m.Acquire(context.Background(), "s")
		done <- e
	}()
	select {
	case <-done:
		t.Fatal("third acquire returned too early")
	case <-time.After(80 * time.Millisecond):
	}
	r1()
	select {
	case e := <-done:
		if e != nil {
			t.Fatalf("expected ok, got %v", e)
		}
	case <-time.After(time.Second):
		t.Fatal("third acquire did not unblock after release")
	}
	r2()
}

func TestQueueTimeout(t *testing.T) {
	m := New(50, 100, 1)
	r, err := m.Acquire(context.Background(), "s")
	if err != nil {
		t.Fatal(err)
	}
	defer r()
	if _, err := m.Acquire(context.Background(), "s"); !errors.Is(err, ErrTimeout) {
		t.Fatalf("expected ErrTimeout, got %v", err)
	}
}

func TestQueueFull(t *testing.T) {
	m := New(5000, 2, 1)
	r1, _ := m.Acquire(context.Background(), "s")
	defer r1()
	// fill the queue.
	go func() { _, _ = m.Acquire(context.Background(), "s") }()
	go func() { _, _ = m.Acquire(context.Background(), "s") }()
	time.Sleep(20 * time.Millisecond)
	if _, err := m.Acquire(context.Background(), "s"); !errors.Is(err, ErrFull) {
		t.Fatalf("expected ErrFull, got %v", err)
	}
}
