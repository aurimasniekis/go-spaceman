package botapi_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/aurimasniekis/go-spaceman/internal/telegram/botapi"
)

// SM-TG-002 (TG-13-1): the client's backstop for group mode. Files are above the summary content level, so no file
// send (document, photo, album) ever goes to a group, supergroup or channel; the request never leaves the client.
func TestFilesNeverGoToAGroup(t *testing.T) {
	srv, c := newPair(t, noLimits())
	ctx := context.Background()
	file := func() *botapi.InputFile { return &botapi.InputFile{Name: "a.txt", Reader: strings.NewReader("secret")} }
	for _, chat := range []int64{-100123, -5, 0} {
		_, err := c.SendDocument(ctx, botapi.SendDocumentParams{ChatID: chat, Document: file()})
		if !errors.Is(err, botapi.ErrGroupContent) {
			t.Fatalf("sendDocument to %d: %v", chat, err)
		}
		_, err = c.SendPhoto(ctx, botapi.SendPhotoParams{ChatID: chat, Photo: file()})
		if !errors.Is(err, botapi.ErrGroupContent) {
			t.Fatalf("sendPhoto to %d: %v", chat, err)
		}
		_, err = c.SendMediaGroup(ctx, botapi.SendMediaGroupParams{ChatID: chat, Media: []botapi.InputMedia{
			{Type: botapi.MediaDocument, Media: file()}, {Type: botapi.MediaDocument, Media: file()}}})
		if !errors.Is(err, botapi.ErrGroupContent) {
			t.Fatalf("sendMediaGroup to %d: %v", chat, err)
		}
	}
	if calls := srv.Calls("sendDocument", "sendPhoto", "sendMediaGroup"); len(calls) != 0 {
		t.Fatalf("reached the server: %+v", calls)
	}
	// A private chat still gets files.
	if _, err := c.SendDocument(ctx, botapi.SendDocumentParams{ChatID: 42, Document: file()}); err != nil {
		t.Fatal(err)
	}
}
