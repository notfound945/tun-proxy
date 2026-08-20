package system

import (
	"errors"
	"fmt"
	"sync"
)

type rollbackStep struct {
	name string
	undo func() error
}

// Transaction records successful mutations and rolls them back in strict
// reverse order. It is safe to call Rollback more than once.
type Transaction struct {
	mutex      sync.Mutex
	steps      []rollbackStep
	rolledBack bool
}

func (t *Transaction) Apply(name string, apply, undo func() error) error {
	t.mutex.Lock()
	defer t.mutex.Unlock()
	if t.rolledBack {
		return errors.New("transaction has already been rolled back")
	}
	if apply == nil || undo == nil {
		return fmt.Errorf("transaction step %q requires apply and undo functions", name)
	}
	if err := apply(); err != nil {
		return fmt.Errorf("apply %s: %w", name, err)
	}
	t.steps = append(t.steps, rollbackStep{name: name, undo: undo})
	return nil
}

func (t *Transaction) Rollback() error {
	t.mutex.Lock()
	defer t.mutex.Unlock()
	if t.rolledBack {
		return nil
	}
	t.rolledBack = true

	var failures []error
	for index := len(t.steps) - 1; index >= 0; index-- {
		step := t.steps[index]
		if err := step.undo(); err != nil {
			failures = append(failures, fmt.Errorf("rollback %s: %w", step.name, err))
		}
	}
	return errors.Join(failures...)
}
