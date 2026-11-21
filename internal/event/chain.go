package event

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
)

// links e to the event before it, so changing, dropping or inserting an event
// breaks every hash after it. prev is "" for the first one
func ChainHash(prev string, e Event) (string, error) {
	data := []byte(e.Data)
	if len(data) == 0 {
		data = []byte("{}")
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, data); err != nil {
		return "", err
	}

	h := sha256.New()
	str := func(s string) {
		binary.Write(h, binary.BigEndian, uint64(len(s)))
		h.Write([]byte(s))
	}
	str(prev)
	binary.Write(h, binary.BigEndian, e.Seq)
	binary.Write(h, binary.BigEndian, e.TS.UnixMilli())
	str(string(e.Kind))
	str(e.SessionID)
	str(e.ProjectID)
	str(e.Harness)
	str(string(e.Source))
	str(compact.String())
	return hex.EncodeToString(h.Sum(nil)), nil
}
