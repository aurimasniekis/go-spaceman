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

// GoneTopics makes every send into a topic of chatID (any thread, even one just created) answer "message thread not
// found", like a chat whose topics all vanish. Sends without a thread still work.
func (s *Server) GoneTopics(chatID int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.goneAll[chatID] = true
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

// SetBotTopics sets what getMe reports about topics: has_topics_enabled (topic mode on in private chats) and
// allows_users_to_create_topics (users may create and delete topics; BotFather's setting). It changes nothing else:
// createForumTopic keeps working as DisableTopics and the rest of the fake say.
func (s *Server) SetBotTopics(hasTopics, allowsUserTopics bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.opts.Bot.HasTopicsEnabled = hasTopics
	s.opts.Bot.AllowsUsersToCreateTopics = allowsUserTopics
}

// MessagesIn returns the messages in chatID that landed in thread (sent and not deleted, oldest first). Thread 0 is the
// chat without a topic: a send without message_thread_id.
func (s *Server) MessagesIn(chatID int64, thread int) []botapi.Message {
	var out []botapi.Message
	for _, m := range s.Messages(chatID) {
		if m.MessageThreadID == thread {
			out = append(out, m)
		}
	}
	return out
}

// Pinned returns the messages pinned in chatID, oldest pin first.
func (s *Server) Pinned(chatID int64) []int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]int(nil), s.pinned[chatID]...)
}

// editForumTopicLocked renames a topic (name 0–128 characters; empty keeps the name). A topic that doesn't exist, or was
// deleted, can't be edited; a closed one can.
func (s *Server) editForumTopicLocked(p params) (any, *apiError) {
	chat, e := s.targetLocked(p)
	if e != nil {
		return nil, e
	}
	n, ok := p.int64("message_thread_id")
	if !ok {
		return nil, badRequest("message thread not found")
	}
	t := s.topics[chat.ID][int(n)]
	if t == nil || t.absent {
		return nil, badRequest("message thread not found")
	}
	name := p.str("name")
	if utf16Len(name) > 128 {
		return nil, badRequest("topic name is invalid")
	}
	if name != "" {
		t.name = name
	}
	return true, nil
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
	if s.goneAll[chatID] {
		return &apiError{code: http.StatusBadRequest, desc: "Bad Request: message thread not found"}
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
