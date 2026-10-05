package runlog

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// TasksDir holds the one-off tasks the owner pushed to the machine: a brief
// each, in a file named for the task. A task waits in the directory of the
// worker it is for, or in TaskAny when any worker may take it, and moves to
// TaskTaken when a worker is handed it.
const (
	TasksDir  = "tasks"
	TaskAny   = "_any"
	TaskTaken = "_taken"
	taskExt   = ".md"
)

var taskIDRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// ValidTaskID reports whether id can name a task's file.
func ValidTaskID(id string) bool { return taskIDRE.MatchString(id) }

// Task is a one-off task: work the owner asks for directly, with a brief of
// its own, outside the queue of issues.
type Task struct {
	ID string
	// Worker is the worker the owner chose; empty when any worker may take it.
	Worker string
	Brief  string
	// Queued is when the task reached the machine.
	Queued time.Time
}

// PendingTask returns the oldest task that waits for this worker: one pushed
// to it by name first, then one for any worker.
func PendingTask(dir, worker string) (Task, bool) {
	for _, where := range []string{worker, TaskAny} {
		entries, err := os.ReadDir(filepath.Join(dir, TasksDir, where))
		if err != nil {
			continue
		}
		var ids []string
		for _, e := range entries {
			if id, ok := strings.CutSuffix(e.Name(), taskExt); ok && !e.IsDir() && ValidTaskID(id) {
				ids = append(ids, id)
			}
		}
		// An id starts with the time it was pushed, so the oldest sorts first.
		sort.Strings(ids)
		for _, id := range ids {
			path := filepath.Join(dir, TasksDir, where, id+taskExt)
			brief, err := os.ReadFile(path)
			info, statErr := os.Stat(path)
			if err != nil || statErr != nil {
				continue
			}
			task := Task{ID: id, Brief: string(brief), Queued: info.ModTime()}
			if where != TaskAny {
				task.Worker = where
			}
			return task, true
		}
	}
	return Task{}, false
}

// TakeTask marks a pending task as handed to a worker.
func TakeTask(dir string, t Task) error {
	where := t.Worker
	if where == "" {
		where = TaskAny
	}
	taken := filepath.Join(dir, TasksDir, TaskTaken)
	if err := os.MkdirAll(taken, 0o755); err != nil {
		return fmt.Errorf("task %s: %w", t.ID, err)
	}
	if err := os.Rename(filepath.Join(dir, TasksDir, where, t.ID+taskExt), filepath.Join(taken, t.ID+taskExt)); err != nil {
		return fmt.Errorf("task %s: %w", t.ID, err)
	}
	return nil
}

// ReturnTask puts a task that a worker was handed back in that worker's
// queue: its session ended before the task was done.
func ReturnTask(dir, id, worker string) error {
	queue := filepath.Join(dir, TasksDir, worker)
	if err := os.MkdirAll(queue, 0o755); err != nil {
		return fmt.Errorf("task %s: %w", id, err)
	}
	if err := os.Rename(filepath.Join(dir, TasksDir, TaskTaken, id+taskExt), filepath.Join(queue, id+taskExt)); err != nil {
		return fmt.Errorf("task %s: %w", id, err)
	}
	return nil
}

// TakenTask returns the brief of a task that a worker was handed.
func TakenTask(dir, id string) (string, bool) {
	brief, err := os.ReadFile(filepath.Join(dir, TasksDir, TaskTaken, id+taskExt))
	return string(brief), err == nil
}

// TaskInHand returns the one-off task the worker was handed and has not
// finished; "" for none. A task is over when the worker is next found done or
// is handed an issue, and when its session ends.
func TaskInHand(checkins []Checkin, worker string) string {
	task := ""
	for _, c := range checkins {
		switch {
		case c.Worker != worker:
		case c.Sent && c.Task != "":
			task = c.Task
		case c.Verdict == VerdictStarted || c.Verdict == VerdictRecycled || c.Verdict == VerdictDone || (c.Sent && c.Issue != 0):
			task = ""
		}
	}
	return task
}

// TaskWorker returns the worker that was last handed the task; "" when the
// log does not say.
func TaskWorker(checkins []Checkin, id string) string {
	worker := ""
	for _, c := range checkins {
		if c.Sent && c.Task == id {
			worker = c.Worker
		}
	}
	return worker
}
