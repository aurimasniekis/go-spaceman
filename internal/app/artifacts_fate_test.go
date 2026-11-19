package app

import (
	"database/sql"
	"errors"
	"testing"

	"github.com/aurimasniekis/go-spaceman/internal/fleet"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
)

func TestRunFate(t *testing.T) {
	boom := errors.New("db down")
	if f, err := runFate(db.Run{}, sql.ErrNoRows); f != fleet.RunAbsent || err != nil {
		t.Fatalf("not found: %v %v", f, err)
	}
	if f, err := runFate(db.Run{}, boom); f != fleet.RunKeep || err == nil {
		t.Fatalf("error must not read as absent: %v %v", f, err)
	}
	if f, err := runFate(db.Run{}, nil); f != fleet.RunKeep || err != nil {
		t.Fatalf("existing run: %v %v", f, err)
	}
	if f, err := runFate(db.Run{RecordingDeletedAt: sql.NullInt64{Int64: 1, Valid: true}}, nil); f != fleet.RunRecordingDeleted || err != nil {
		t.Fatalf("deleted recording: %v %v", f, err)
	}
}
