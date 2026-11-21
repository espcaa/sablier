package harness

import (
	"time"

	"github.com/espcaa/sablier/internal/event"
	"github.com/espcaa/sablier/internal/prompt"
)

type Emitter struct {
	Harness string
	Session string
	Project string
	Salt    []byte // for prompt hashes
}

func (em Emitter) Event(kind event.Kind, at time.Time, data any) event.Event {
	e, err := event.New(kind, at, data)
	if err != nil {
		panic(err) // unknown kind, a bug in the adapter
	}
	e.SessionID, e.ProjectID, e.Harness, e.Source = em.Session, em.Project, em.Harness, event.SourceAdapter
	return e
}

// a prompt_sent with its hash. the text goes to the local db only, never to the server
func (em Emitter) Prompt(at time.Time, text string, queued bool) event.Event {
	e := em.Event(event.PromptSent, at, event.PromptSentData{
		Hash:         prompt.Hash(prompt.Key(em.Salt, em.Session, at), text),
		ApprovalLike: prompt.ApprovalLike(text),
		Queued:       queued,
	})
	e.Prompt = text
	return e
}
