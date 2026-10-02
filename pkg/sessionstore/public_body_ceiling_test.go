package sessionstore

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/looprig/harness/pkg/event"
	durablestore "github.com/looprig/sessionstore"
)

// TestPublicBodyCeilingMatchesHarnessCodecs is the public sibling of
// TestRuntimeBodyLimitMatchesHarnessCodecs. A public body is a projection of a
// runtime event body no larger than it, so the event codec's ceiling bounds
// every public body this journal can commit. SessionStore's public read
// resolves an object-backed public body only up to MaxObjectPublicBodyBytes;
// if the codec ceiling ever rose above it, Harness could commit a public body
// no public read would serve.
func TestPublicBodyCeilingMatchesHarnessCodecs(t *testing.T) {
	_, err := event.UnmarshalEvent(make([]byte, maxRuntimeBodyBytes+1))
	var limit *event.EventLimitError
	if !errors.As(err, &limit) {
		t.Fatalf("event decode of an oversized body = (%T %v), want *event.EventLimitError", err, err)
	}
	if limit.Max > durablestore.MaxObjectPublicBodyBytes {
		t.Fatalf("event codec ceiling %d exceeds SessionStore's object-backed public body ceiling %d",
			limit.Max, durablestore.MaxObjectPublicBodyBytes)
	}
	if maxRuntimeBodyBytes > durablestore.MaxObjectPublicBodyBytes {
		t.Fatalf("runtime body ceiling %d exceeds SessionStore's object-backed public body ceiling %d",
			maxRuntimeBodyBytes, durablestore.MaxObjectPublicBodyBytes)
	}
}

// TestOffloadedPublicBodyRoundTripsThroughThePublicRead commits a public event
// above the released inline ceiling under the DEFAULT offload threshold -- so
// its public body is held in an object, the state every such body is in -- and
// reads it back through SessionStore's public read byte-identical to the body
// the commit handed the live delivery.
func TestOffloadedPublicBodyRoundTripsThroughThePublicRead(t *testing.T) {
	for name, size := range map[string]int{
		"just above the inline ceiling": durablestore.MaxInlineBodyBytes + 1,
		"above the page budget":         2 * durablestore.DefaultJournalPageBytes,
	} {
		t.Run(name, func(t *testing.T) {
			r := newCommittedRig(t)
			value := publicEventWithEncodedSize(t, r.sessionID, size)
			if err := r.hub.PublishEvent(context.Background(), value); err != nil {
				t.Fatalf("PublishEvent() error = %v", err)
			}
			host := recvWithin(t, r.committed)
			if len(host.PublicBody) <= durablestore.MaxInlineBodyBytes {
				t.Fatalf("committed public body = %d bytes, want above the %d-byte inline ceiling",
					len(host.PublicBody), durablestore.MaxInlineBodyBytes)
			}

			page, err := r.store.durable.ReadPublicJournal(context.Background(), durablestore.ReadPublicJournalRequest{
				TenantID: harnessTenantID, SessionID: harnessSessionID(r.sessionID), FromSeq: host.JournalSeq,
			})
			if err != nil {
				t.Fatalf("ReadPublicJournal() error = %v", err)
			}
			if len(page.Events) != 1 || page.Events[0].JournalSeq != host.JournalSeq {
				t.Fatalf("public page = %d events, want the one at sequence %d", len(page.Events), host.JournalSeq)
			}
			stored := page.Events[0]
			if string(stored.EventID) != host.EventID {
				t.Fatalf("stored EventID = %q, want the committed %q", stored.EventID, host.EventID)
			}
			if !bytes.Equal(stored.Body, host.PublicBody) {
				t.Fatalf("stored public body (%d bytes) is not byte-identical to the committed one (%d bytes)",
					len(stored.Body), len(host.PublicBody))
			}

			// The body really was offloaded: the record carries a reference.
			envelope := readDurableEnvelopeAt(t, r, host.JournalSeq)
			if envelope.Public.Reference == nil || envelope.Public.Inline != nil {
				t.Fatalf("public slot = %+v, want an object reference", envelope.Public)
			}
		})
	}
}

func readDurableEnvelopeAt(t *testing.T, r *committedRig, seq uint64) durablestore.Envelope {
	t.Helper()
	cursor, err := r.backend.Ledger.Read(context.Background(), ledgerName(r.sessionID), seq)
	if err != nil {
		t.Fatalf("Ledger.Read() error = %v", err)
	}
	defer func() { _ = cursor.Close() }()
	stored, err := cursor.Next(context.Background())
	if err != nil {
		t.Fatalf("Cursor.Next() error = %v", err)
	}
	envelope, err := durablestore.DecodeEnvelope(stored.Payload)
	if err != nil {
		t.Fatalf("DecodeEnvelope() error = %v", err)
	}
	return envelope
}
