package runlog

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// QueuesDir holds each worker's queue as the agent last read it, one file
// per worker.
const QueuesDir = "queues"

// QueueView is a worker's queue as the agent computed it on its machine: what
// it would hand over, and why it holds the rest. `shed status` and `shed
// queue` read it, so the owner sees the agent's reasons and not a guess.
type QueueView struct {
	Time   time.Time    `json:"time"`
	Worker string       `json:"worker"`
	Items  []QueueEntry `json:"items"`
	// Reserve says what became of the worker's backfill sources at this
	// reading: listed, and what they offered, or why they were not listed.
	// Empty when the worker has none.
	Reserve string `json:"reserve,omitempty"`
}

// QueueEntry is one item of a worker's queue.
type QueueEntry struct {
	Repo   string `json:"repo"`
	Number int    `json:"number"`
	Title  string `json:"title"`
	// Kind says what the item is when it is not new work: rework, look,
	// verify, resume (the owner answered), stack, bumped.
	Kind string `json:"kind,omitempty"`
	// Hold says why a worker cannot act on the item now; empty when one can.
	Hold     string `json:"hold,omitempty"`
	Backfill bool   `json:"backfill,omitempty"`
}

// Workable counts the items a worker can act on now.
func (v QueueView) Workable() int {
	n := 0
	for _, e := range v.Items {
		if e.Hold == "" {
			n++
		}
	}
	return n
}

// WriteQueue keeps the worker's queue for `shed status` and `shed queue`. The
// file is replaced in one step, so a reader never sees half of it.
func WriteQueue(dir string, v QueueView) error {
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("encode the queue of %s: %w", v.Worker, err)
	}
	queues := filepath.Join(dir, QueuesDir)
	if err := os.MkdirAll(queues, 0o755); err != nil {
		return fmt.Errorf("create queues directory: %w", err)
	}
	tmp, err := os.CreateTemp(queues, "."+v.Worker+"-*")
	if err != nil {
		return fmt.Errorf("write the queue of %s: %w", v.Worker, err)
	}
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return fmt.Errorf("write the queue of %s: %w", v.Worker, err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return fmt.Errorf("write the queue of %s: %w", v.Worker, err)
	}
	if err := os.Rename(tmp.Name(), filepath.Join(queues, v.Worker)); err != nil {
		os.Remove(tmp.Name())
		return fmt.Errorf("write the queue of %s: %w", v.Worker, err)
	}
	return nil
}

// ParseQueue reads a queue as WriteQueue wrote it.
func ParseQueue(line string) (QueueView, error) {
	var v QueueView
	if err := json.Unmarshal([]byte(line), &v); err != nil {
		return QueueView{}, fmt.Errorf("parse queue: %w", err)
	}
	return v, nil
}
