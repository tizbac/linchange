package sfu

import (
	"testing"
	"time"
)

func TestJoinOffersAndSpeakingIsCapped(t *testing.T) {
	m := New(Options{})
	defer m.Close()
	signals := make(chan Signal, 8)
	if err := m.Join("meeting", "alice", func(s Signal) { signals <- s }); err != nil {
		t.Fatalf("Join: %v", err)
	}
	deadline := time.After(2 * time.Second)
	for {
		select {
		case signal := <-signals:
			if signal.Type != "sfu-offer" {
				continue
			}
			if len(signal.SDP) == 0 {
				t.Fatalf("offer = %+v, want SDP", signal)
			}
			goto offered
		case <-deadline:
			t.Fatal("Join did not send an offer")
		}
	}
offered:
	if got := m.Participants("meeting"); len(got) != 1 || got[0] != "alice" {
		t.Fatalf("participants = %v, want alice", got)
	}
	m.Speaking("meeting", "alice", true)
	deadline = time.After(2 * time.Second)
	for {
		select {
		case signal := <-signals:
			if signal.Type != "sfu-audio" {
				continue
			}
			if !signal.Allowed {
				t.Fatalf("speaking signal = %+v, want allowed audio", signal)
			}
			goto spoken
		case <-deadline:
			t.Fatal("Speaking did not answer")
		}
	}
spoken:
	m.Leave("meeting", "alice")
	if got := m.Participants("meeting"); len(got) != 0 {
		t.Fatalf("participants after leave = %v, want none", got)
	}
}
