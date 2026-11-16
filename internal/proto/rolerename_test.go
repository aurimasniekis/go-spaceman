package proto

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// ADR-0093 T-H2: memory.rename, memory.renamed and memory.renamed.ack round-trip through an envelope, and their
// identifiers and results are checked before any side effect.
func TestRoleRenameMessages(t *testing.T) {
	id, space := "rrn_"+strings.Repeat("a", 26), "spc_"+strings.Repeat("b", 26)
	now := time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)
	cmd := Command{CommandID: "cmd_" + strings.Repeat("c", 26), Kind: CmdMemoryRename, SpaceID: space, Generation: 3, IdempotencyKey: "role-rename:" + id,
		Payload: mustMarshal(t, MemoryRename{RenameID: id, SpaceID: space, From: "implementor", To: "implementor-hard"})}
	if err := cmd.Validate(); err != nil {
		t.Fatalf("memory.rename without a run: %v", err)
	}
	var back Command
	roundTrip(t, TypeCommand, now, cmd, &back)
	var p MemoryRename
	if err := json.Unmarshal(back.Payload, &p); err != nil || p.Validate() != nil || p.From != "implementor" || p.To != "implementor-hard" || p.RenameID != id {
		t.Fatalf("payload %+v %v", p, err)
	}
	for name, mut := range map[string]func(*MemoryRename){
		"id":    func(m *MemoryRename) { m.RenameID = "rrn_x" },
		"space": func(m *MemoryRename) { m.SpaceID = "../x" },
		"from":  func(m *MemoryRename) { m.From = "../x" },
		"to":    func(m *MemoryRename) { m.To = "Bad" },
		"same":  func(m *MemoryRename) { m.To = m.From },
	} {
		m := p
		mut(&m)
		if m.Validate() == nil {
			t.Errorf("%s: invalid payload accepted", name)
		}
	}

	rep := MemoryRenamed{RenameID: id, SpaceID: space, Result: MemoryRenameDeferred, Message: "a run of the role is active"}
	var gotRep MemoryRenamed
	roundTrip(t, TypeMemoryRenamed, now, rep, &gotRep)
	if gotRep != rep || gotRep.Validate() != nil {
		t.Fatalf("report %+v", gotRep)
	}
	for _, r := range []string{MemoryRenameOK, MemoryRenameDeferred, MemoryRenameError} {
		if (MemoryRenamed{RenameID: id, SpaceID: space, Result: r}).Validate() != nil {
			t.Errorf("result %s refused", r)
		}
	}
	for name, bad := range map[string]MemoryRenamed{
		"result": {RenameID: id, SpaceID: space, Result: "moved"},
		"id":     {RenameID: "x", SpaceID: space, Result: MemoryRenameOK},
		"space":  {RenameID: id, SpaceID: "x", Result: MemoryRenameOK},
	} {
		if bad.Validate() == nil {
			t.Errorf("%s: invalid report accepted", name)
		}
	}
	ack := MemoryRenamedAck{RenameID: id, Result: MemoryRenameOK}
	var gotAck MemoryRenamedAck
	roundTrip(t, TypeMemoryRenamedAck, now, ack, &gotAck)
	if gotAck != ack {
		t.Fatalf("ack %+v", gotAck)
	}
	if MaxMinor < MinorRoleRename {
		t.Fatalf("MaxMinor %d below MinorRoleRename %d", MaxMinor, MinorRoleRename)
	}
}

func mustMarshal(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func roundTrip(t *testing.T, typ string, now time.Time, in, out any) {
	t.Helper()
	b, err := Encode("m1", typ, now, in)
	if err != nil {
		t.Fatal(err)
	}
	e, err := Decode(b)
	if err != nil || e.Type != typ {
		t.Fatalf("%+v %v", e, err)
	}
	if err := e.Decode(out); err != nil {
		t.Fatal(err)
	}
}
