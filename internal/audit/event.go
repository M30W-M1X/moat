// Package audit records a durable, tamper-evident log of every boundary a
// Moat sandbox crosses. Events are metadata only. The log never holds a
// secret, a payload, or command output.
package audit

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"
)

// Event is one recorded boundary crossing. Attrs is type-specific metadata.
type Event struct {
	Seq     uint64
	TS      time.Time
	Type    string
	Sandbox string // sandbox id, or "" for a daemon-level event
	Token   string // token id, never the secret
	Attrs   map[string]any
	Prev    string // hash of the previous event
	Hash    string // hash of this event's canonical form

	// flush marks an internal sentinel event. The async writer closes this
	// channel when it reads the sentinel from the queue. canonical and
	// marshalLine build explicit maps, so this field never reaches the log.
	flush chan struct{}
}

// The closed set of event types for v1.
const (
	TypeDaemonStart    = "daemon.start"
	TypeDaemonStop     = "daemon.stop"
	TypeSandboxCreate  = "sandbox.create"
	TypeSandboxDestroy = "sandbox.destroy"
	TypeExec           = "exec"
	TypeFilePut        = "file.put"
	TypeFileGet        = "file.get"
	TypeSecretBlock    = "secret.block"
	TypeSecretAllow    = "secret.allow"
	TypeSecretRedact   = "secret.redact"
	TypeEgressAllow    = "egress.allow"
	TypeEgressDeny     = "egress.deny"
	TypeQuotaReject    = "quota.reject"
	TypeOrphanReap     = "orphan.reap"
)

const zeroHash = "0000000000000000000000000000000000000000000000000000000000000000"

// canonical serialises e with keys sorted at every level and the hash field
// omitted. encoding/json already sorts map keys and struct fields marshal in
// declaration order, so the field order below is the canonical order.
func canonical(e Event) ([]byte, error) {
	m := map[string]any{
		"seq":     e.Seq,
		"ts":      e.TS.UTC().Format(time.RFC3339Nano),
		"type":    e.Type,
		"sandbox": e.Sandbox,
		"token":   e.Token,
		"attrs":   e.Attrs,
		"prev":    e.Prev,
	}
	if e.Attrs == nil {
		m["attrs"] = map[string]any{}
	}
	b, err := json.Marshal(m) // json.Marshal sorts map keys
	if err != nil {
		return nil, fmt.Errorf("canonical event: %w", err)
	}
	return b, nil
}

// hashEvent returns the hex sha256 of e's canonical form.
func hashEvent(e Event) (string, error) {
	b, err := canonical(e)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}
