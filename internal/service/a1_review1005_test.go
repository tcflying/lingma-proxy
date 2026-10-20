package service

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestA1Review1005DeadlineTextCannotReplay(t *testing.T) {
	for _, text := range []string{"unexpected eof", "broken pipe"} {
		t.Run(text, func(t *testing.T) {
			p := a1Peer(t)
			p.onPrompt = func(p *ipcPeer, id string, _ int) { p.chunk(id, text) }
			s := newIPCService(t, p, Config{SessionMode: SessionModeReuse, Timeout: 100 * time.Millisecond})
			parent := context.Background()
			result, err := s.Generate(parent, chatRequest("fixture"))
			count := len(p.promptTexts())
			t.Logf("prompts=%d result_nil=%v deadline=%v parent_err=%v sticky=%q deleted=%d", count, result == nil, errors.Is(err, context.DeadlineExceeded), parent.Err(), s.stickySessionForTest(), len(p.deletedSessions()))
			if parent.Err() != nil || !errors.Is(err, context.DeadlineExceeded) || result != nil || count != 1 || s.stickySessionForTest() != "" {
				t.Fatalf("internal deadline must not replay partial text: prompts=%d err=%v", count, err)
			}
		})
	}
}
