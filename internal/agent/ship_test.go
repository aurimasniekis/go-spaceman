package agent

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/proto"
)

func TestShipPartsStayInsideAMessage(t *testing.T) {
	raw := func(n int) json.RawMessage { return json.RawMessage(`"` + strings.Repeat("x", n-2) + `"`) }
	// 22 MB of output in one flush interval, as a burst of 64 KiB records: no message may pass MaxMessage.
	var raws []json.RawMessage
	for range 22 << 20 >> 16 {
		raws = append(raws, raw(64<<10*4/3))
	}
	parts := shipParts(raws, shipChunkBytes)
	n := 0
	for _, p := range parts {
		size := 0
		for _, r := range p {
			size += len(r)
		}
		if size > shipChunkBytes {
			t.Fatalf("part of %d bytes", size)
		}
		b, err := proto.Encode("msg_x", proto.TypeRecording, time.Now(), proto.RecordingChunk{RunID: "run_x", SpaceID: "spc_x", Records: p})
		if err != nil || len(b) > proto.MaxMessage {
			t.Fatalf("message of %d bytes: %v", len(b), err)
		}
		n += len(p)
	}
	if n != len(raws) || len(parts) < 20 {
		t.Fatalf("%d records in %d parts", n, len(parts))
	}
	// Order is kept, a record larger than the limit goes alone, and nothing gives no parts.
	parts = shipParts([]json.RawMessage{raw(10), raw(30), raw(10), raw(10)}, 20)
	if len(parts) != 3 || len(parts[0]) != 1 || len(parts[1]) != 1 || len(parts[2]) != 2 {
		t.Fatalf("parts %v", parts)
	}
	if shipParts(nil, 20) != nil {
		t.Fatal("parts of nothing")
	}
}
