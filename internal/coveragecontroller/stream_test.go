package coveragecontroller

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func nativeItem(n uint64, method, item string) frame {
	stamp := "startedAtMs"
	if method == "item/completed" {
		stamp = "completedAtMs"
	}
	return frame{testConnection, n, notificationFrame, []byte(fmt.Sprintf(`{"method":%q,"params":{"threadId":"thread","turnId":"turn","item":{"type":"contextCompaction","id":%q},%q:123}}`, method, item, stamp))}
}
func testStream(t *testing.T) *stream {
	t.Helper()
	s, err := newStream(testConnection, "thread", 8, 8192, 2048, 8, 2)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestStreamThreadBoundaryRequiresExactIdentity(t *testing.T) {
	for _, method := range []string{"thread/closed", "thread/archived", "thread/deleted", "thread/reverted", "thread/settings/updated"} {
		for _, params := range []string{`{"threadId":"other"}`, `{}`, `{"threadId":null}`, `{"threadId":""}`, `{"threadId":1}`} {
			s := testStream(t)
			raw := []byte(fmt.Sprintf(`{"method":%q,"params":%s}`, method, params))
			err := s.accept(frame{testConnection, 1, notificationFrame, raw})
			foreign := params == `{"threadId":"other"}`
			if foreign && err != nil || !foreign && err == nil {
				t.Fatalf("method=%s params=%s err=%v", method, params, err)
			}
			if !foreign && s.cause == nil {
				t.Fatal("malformed thread boundary did not invalidate stream")
			}
		}
	}
}

func TestStreamMergesRPCBeforeCompletingNativeLifecycle(t *testing.T) {
	s := testStream(t)
	if err := s.accept(nativeItem(3, "item/completed", "item")); err != nil {
		t.Fatal(err)
	}
	if err := s.accept(nativeItem(1, "item/started", "item")); err != nil {
		t.Fatal(err)
	}
	if len(s.candidates) != 0 {
		t.Fatal("published across missing response")
	}
	if err := s.accept(frame{testConnection, 2, responseFrame, []byte(`{"result":{}}`)}); err != nil {
		t.Fatal(err)
	}
	n, err := s.prefix.frontier()
	if err != nil || n != 3 || len(s.candidates) != 1 {
		t.Fatal(n, err, s.candidates)
	}
	if s.candidates[0].startOrdinal != 1 || s.candidates[0].completedOrdinal != 3 {
		t.Fatal(s.candidates)
	}
}

func TestStreamRejectsAmbiguousMalformedAndUnhandledEvents(t *testing.T) {
	valid := string(nativeItem(1, "item/started", "item").raw)
	for _, raw := range []string{
		strings.Replace(valid, `"threadId":"thread"`, `"threadId":"other","threadId":"thread"`, 1),
		strings.Replace(valid, `"startedAtMs":123`, `"startedAtMs":null`, 1),
		strings.Replace(valid, `"startedAtMs":123`, `"startedAtMs":1.5`, 1),
		strings.Replace(valid, `"id":"item"`, `"id":""`, 1),
		valid + ` {}`, `null`, strings.Repeat("[", 66) + strings.Repeat("]", 66),
	} {
		s := testStream(t)
		if err := s.accept(frame{testConnection, 1, notificationFrame, []byte(raw)}); err == nil {
			t.Fatal("accepted", raw)
		}
		if len(s.candidates) != 0 {
			t.Fatal("candidate survived malformed input")
		}
	}
	s := testStream(t)
	if err := s.accept(frame{testConnection, 1, serverRequestFrame, []byte(`{"method":"approval","id":1}`)}); !errors.Is(err, errServerRequest) {
		t.Fatal(err)
	}
}

func TestStreamCandidateBoundAndLossDiscardUnstagedResults(t *testing.T) {
	s := testStream(t)
	for i := uint64(1); i <= 2; i++ {
		if err := s.accept(nativeItem(i*2-1, "item/started", fmt.Sprint(i))); err != nil {
			t.Fatal(err)
		}
		if err := s.accept(nativeItem(i*2, "item/completed", fmt.Sprint(i))); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.accept(nativeItem(5, "item/started", "third")); err != nil {
		t.Fatal(err)
	}
	if err := s.accept(nativeItem(6, "item/completed", "third")); !errors.Is(err, errCandidateLimit) {
		t.Fatal(err)
	}
	if len(s.candidates) != 0 || len(s.prefix.pending) != 0 || len(s.lifecycles.items) != 0 {
		t.Fatal("invalid data retained")
	}
}

func TestStreamRemoteErrorOccupiesOrdinalButCannotFillMissingStart(t *testing.T) {
	s := testStream(t)
	if err := s.accept(frame{testConnection, 1, responseFrame, []byte(`{"error":{"code":-1,"message":"rejected"}}`)}); err != nil {
		t.Fatal(err)
	}
	if err := s.accept(nativeItem(2, "item/completed", "item")); !errors.Is(err, errLifecycleOrder) {
		t.Fatal(err)
	}
}
