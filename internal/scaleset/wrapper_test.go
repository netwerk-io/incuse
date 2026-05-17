package scaleset

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"

	ssapi "github.com/actions/scaleset"
)

type fakeListenerClient struct {
	mu        sync.Mutex
	msg       *ssapi.RunnerScaleSetMessage
	msgErr    error
	deleteIDs []int
	acquired  []int64
	session   ssapi.RunnerScaleSetSession
}

func (f *fakeListenerClient) GetMessage(_ context.Context, _, _ int) (*ssapi.RunnerScaleSetMessage, error) {
	return f.msg, f.msgErr
}

func (f *fakeListenerClient) DeleteMessage(_ context.Context, id int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleteIDs = append(f.deleteIDs, id)
	return nil
}

func (f *fakeListenerClient) AcquireJobs(_ context.Context, ids []int64) ([]int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.acquired = append(f.acquired, ids...)
	return ids, nil
}

func (f *fakeListenerClient) Session() ssapi.RunnerScaleSetSession { return f.session }

type recordingMinter struct {
	mu     sync.Mutex
	events []*ssapi.JobAssigned
	err    error
}

func (r *recordingMinter) Mint(_ context.Context, e *ssapi.JobAssigned) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
	return r.err
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestMintingClient_DispatchesEveryJobAssigned(t *testing.T) {
	minter := &recordingMinter{}
	fc := &fakeListenerClient{
		msg: &ssapi.RunnerScaleSetMessage{
			MessageID: 42,
			JobAssignedMessages: []*ssapi.JobAssigned{
				{JobMessageBase: ssapi.JobMessageBase{RunnerRequestID: 1}},
				{JobMessageBase: ssapi.JobMessageBase{RunnerRequestID: 2}},
				nil,
				{JobMessageBase: ssapi.JobMessageBase{RunnerRequestID: 3}},
			},
		},
	}
	wrapped := &mintingClient{inner: fc, minter: minter, logger: discardLogger()}

	msg, err := wrapped.GetMessage(t.Context(), 0, 10)
	if err != nil {
		t.Fatalf("get message: %v", err)
	}
	if msg == nil || msg.MessageID != 42 {
		t.Fatalf("message not passed through: %+v", msg)
	}
	if len(minter.events) != 3 {
		t.Fatalf("nil entries should be skipped; got %d events", len(minter.events))
	}
	for i, want := range []int64{1, 2, 3} {
		if minter.events[i].RunnerRequestID != want {
			t.Errorf("event %d: want runner_request_id=%d, got %d", i, want, minter.events[i].RunnerRequestID)
		}
	}
}

func TestMintingClient_MintErrorDoesNotPoisonMessage(t *testing.T) {
	minter := &recordingMinter{err: errors.New("kms exploded")}
	fc := &fakeListenerClient{
		msg: &ssapi.RunnerScaleSetMessage{
			MessageID: 7,
			JobAssignedMessages: []*ssapi.JobAssigned{
				{JobMessageBase: ssapi.JobMessageBase{RunnerRequestID: 99}},
			},
		},
	}
	wrapped := &mintingClient{inner: fc, minter: minter, logger: discardLogger()}

	msg, err := wrapped.GetMessage(t.Context(), 0, 1)
	if err != nil {
		t.Fatalf("mint errors must be swallowed; got %v", err)
	}
	if msg == nil || msg.MessageID != 7 {
		t.Fatalf("message dropped on mint error")
	}
	if len(minter.events) != 1 {
		t.Fatalf("event should still have been dispatched once; got %d", len(minter.events))
	}
}

func TestMintingClient_NilMessagePassedThrough(t *testing.T) {
	fc := &fakeListenerClient{msg: nil}
	minter := &recordingMinter{}
	wrapped := &mintingClient{inner: fc, minter: minter, logger: discardLogger()}

	msg, err := wrapped.GetMessage(t.Context(), 0, 1)
	if err != nil || msg != nil {
		t.Fatalf("nil pass-through: msg=%+v err=%v", msg, err)
	}
}

func TestMintingClient_GetMessageErrorPropagated(t *testing.T) {
	fc := &fakeListenerClient{msgErr: errors.New("network broke")}
	minter := &recordingMinter{}
	wrapped := &mintingClient{inner: fc, minter: minter, logger: discardLogger()}

	if _, err := wrapped.GetMessage(t.Context(), 0, 1); err == nil {
		t.Fatal("want upstream error to propagate")
	}
	if len(minter.events) != 0 {
		t.Fatalf("must not dispatch on upstream error; got %d events", len(minter.events))
	}
}

func TestMintingClient_PassThroughMethods(t *testing.T) {
	fc := &fakeListenerClient{}
	wrapped := &mintingClient{inner: fc, minter: &recordingMinter{}, logger: discardLogger()}

	if err := wrapped.DeleteMessage(t.Context(), 123); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if len(fc.deleteIDs) != 1 || fc.deleteIDs[0] != 123 {
		t.Fatalf("delete id not forwarded: %+v", fc.deleteIDs)
	}

	got, err := wrapped.AcquireJobs(t.Context(), []int64{10, 20})
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if len(got) != 2 || got[0] != 10 || got[1] != 20 {
		t.Fatalf("acquire result mismatch: %+v", got)
	}
}
