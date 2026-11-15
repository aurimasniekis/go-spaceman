package settings

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aurimasniekis/go-spaceman/internal/store"
)

// SM-TG-002 (TG-13-1): anything for a group chat is summary whatever the space's content level says.
func TestFullNeverInAGroup(t *testing.T) {
	for _, c := range []struct {
		level   string
		inGroup bool
		want    bool
	}{
		{ContentFull, false, true},
		{ContentFull, true, false},
		{ContentSummary, false, false},
		{ContentSummary, true, false},
		{ContentOff, false, false},
		{"", false, false},
	} {
		if got := Full(Space{ContentLevel: c.level}, c.inGroup); got != c.want {
			t.Errorf("Full(%q, group %v) = %v", c.level, c.inGroup, got)
		}
	}
	for id, want := range map[int64]bool{42: false, 1: false, 0: true, -1: true, -1001234567890: true} {
		if IsGroupChat(id) != want {
			t.Errorf("IsGroupChat(%d) = %v", id, !want)
		}
	}
}

// The summary rule has one choke point: no Telegram code outside this package reads a space's content level itself;
// it asks Full (or Visible). A new renderer that compares ContentLevel directly would bypass the group rule.
func TestContentLevelOnlyThroughFull(t *testing.T) {
	roots := []string{"..", filepath.Join("..", "..", "app"), filepath.Join("..", "..", "api")}
	fset := token.NewFileSet()
	for _, root := range roots {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if filepath.Base(path) == "settings" && filepath.Dir(path) == ".." {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
			if err != nil {
				return err
			}
			ast.Inspect(f, func(n ast.Node) bool {
				if s, ok := n.(*ast.SelectorExpr); ok && s.Sel.Name == "ContentLevel" {
					t.Errorf("%s reads ContentLevel directly; use settings.Full", fset.Position(s.Pos()))
				}
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

// GroupSpace finds a bound group's space from the cache: only for a group chat ID, and only while Telegram may see
// the space.
func TestGroupSpace(t *testing.T) {
	svc, s := newSvc(t, nil)
	ctx := context.Background()
	if _, err := svc.PutSpace(ctx, admin, "spc_a", 0, SpaceInput{Enabled: ptr(true)}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.PutSpace(ctx, admin, "spc_b", 0, SpaceInput{Enabled: ptr(false)}); err != nil {
		t.Fatal(err)
	}
	if err := s.W().TxNamed(ctx, "bind", func(tx *store.Tx) error {
		if _, err := tx.SQL.ExecContext(ctx, `UPDATE space_telegram_settings SET group_chat_id = -100500 WHERE space_id = 'spc_a'`); err != nil {
			return err
		}
		_, err := tx.SQL.ExecContext(ctx, `UPDATE space_telegram_settings SET group_chat_id = -100600 WHERE space_id = 'spc_b'`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := svc.Load(ctx); err != nil {
		t.Fatal(err)
	}
	if sp, ok := svc.GroupSpace(-100500); !ok || sp != "spc_a" {
		t.Fatalf("bound group: %q %v", sp, ok)
	}
	for _, id := range []int64{-100600, -100700, 0, 100500} {
		if sp, ok := svc.GroupSpace(id); ok {
			t.Fatalf("GroupSpace(%d) = %q", id, sp)
		}
	}
	// A settings write keeps the binding (TG-13-2 owns changing it); turning the space off ends it at once.
	if _, err := svc.PutSpace(ctx, admin, "spc_a", 1, SpaceInput{Enabled: ptr(false)}); err != nil {
		t.Fatal(err)
	}
	if _, ok := svc.GroupSpace(-100500); ok {
		t.Fatal("a disabled space's group still acts")
	}
	if svc.Space("spc_a").GroupChatID != -100500 {
		t.Fatalf("the binding was lost: %+v", svc.Space("spc_a"))
	}
}
