package tgtest

import (
	"net/http"
	"strings"

	"github.com/aurimasniekis/go-spaceman/internal/telegram/botapi"
)

type topic struct {
	name           string
	closed, absent bool
}

// DisableTopics makes createForumTopic fail for chatID like a chat whose bot has topics turned off.
func (s *Server) DisableTopics(chatID int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.noTopics[chatID] = true
}

// CloseTopic closes a topic: sends to it fail with TOPIC_CLOSED.
func (s *Server) CloseTopic(chatID int64, thread int) {
	s.setTopic(chatID, thread, func(t *topic) { t.closed = true })
}

// DeleteTopic deletes a topic: sends to it fail with "message thread not found".
func (s *Server) DeleteTopic(chatID int64, thread int) {
	s.setTopic(chatID, thread, func(t *topic) { t.absent = true })
}

func (s *Server) setTopic(chatID int64, thread int, f func(*topic)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if t := s.topics[chatID][thread]; t != nil {
		f(t)
	}
}

// Topics returns the live topics created in a chat, by thread ID.
func (s *Server) Topics(chatID int64) map[int]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[int]string{}
	for id, t := range s.topics[chatID] {
		if !t.absent {
			out[id] = t.name
		}
	}
	return out
}

func (s *Server) createForumTopicLocked(p params) (any, *apiError) {
	chat, e := s.targetLocked(p)
	if e != nil {
		return nil, e
	}
	if s.noTopics[chat.ID] {
		return nil, badRequest("the chat is not a forum")
	}
	name := p.str("name")
	if n := utf16Len(name); n == 0 || n > 128 || strings.TrimSpace(name) == "" {
		return nil, badRequest("topic name is invalid")
	}
	s.nextMsg[chat.ID]++
	id := s.nextMsg[chat.ID]
	if s.topics[chat.ID] == nil {
		s.topics[chat.ID] = map[int]*topic{}
	}
	s.topics[chat.ID][id] = &topic{name: name}
	return botapi.ForumTopic{MessageThreadID: id, Name: name}, nil
}

// threadLocked refuses a send into a topic that was closed or deleted (a thread the fake never created is allowed).
func (s *Server) threadLocked(chatID int64, p params) *apiError {
	n, ok := p.int64("message_thread_id")
	if !ok || n == 0 {
		return nil
	}
	t := s.topics[chatID][int(n)]
	switch {
	case t == nil:
		return nil
	case t.absent:
		return &apiError{code: http.StatusBadRequest, desc: "Bad Request: message thread not found"}
	case t.closed:
		return &apiError{code: http.StatusBadRequest, desc: "Bad Request: TOPIC_CLOSED"}
	}
	return nil
}
