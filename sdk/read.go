package sdk

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// ErrIncomplete is returned by Read when an answer ends without "done" or
// "error", which means the provider stopped half way.
var ErrIncomplete = errors.New("the provider's answer ended early")

// Read decodes the messages of one answer from r and calls fn for each one
// except the final "done". It returns the provider's *Error when the answer
// ends with one, ErrIncomplete when it ends without "done", and fn's error if
// fn fails.
func Read(r io.Reader, fn func(Message) error) error {
	d := json.NewDecoder(r)
	for {
		var m Message
		if err := d.Decode(&m); err != nil {
			if errors.Is(err, io.EOF) {
				return ErrIncomplete
			}
			return fmt.Errorf("reading the provider's answer: %w", err)
		}
		switch {
		case m.Error != nil:
			return m.Error
		case m.Done != nil:
			return nil
		case m == (Message{}):
			// A message this version doesn't know (a newer protocol?): fail
			// rather than drop something that might matter.
			return fmt.Errorf("the provider sent a message this gitopsify doesn't understand; upgrade gitopsify")
		}
		if err := fn(m); err != nil {
			return err
		}
	}
}

// ReadDescription reads the answer to describe.
func ReadDescription(r io.Reader) (*Description, error) {
	var d *Description
	err := Read(r, func(m Message) error {
		if m.Description != nil {
			d = m.Description
		}
		return nil
	})
	if err == nil && d == nil {
		err = fmt.Errorf("the provider answered describe without a description")
	}
	return d, err
}

// ReadCheck reads the answer to check.
func ReadCheck(r io.Reader) (*CheckResult, error) {
	var c *CheckResult
	err := Read(r, func(m Message) error {
		if m.Check != nil {
			c = m.Check
		}
		return nil
	})
	if err == nil && c == nil {
		err = fmt.Errorf("the provider answered check without a result")
	}
	if c != nil && c.Partitions == nil {
		c.Partitions = []Partition{}
	}
	return c, err
}

// ReadInventory reads the answer to discover. onResource, when not nil, is
// called after each resource, for progress.
func ReadInventory(r io.Reader, onResource func(Resource)) (*Inventory, error) {
	inv := &Inventory{}
	err := Read(r, func(m Message) error {
		switch {
		case m.Partition != nil:
			inv.Partitions = append(inv.Partitions, *m.Partition)
		case m.Resource != nil:
			inv.Resources = append(inv.Resources, *m.Resource)
			if onResource != nil {
				onResource(*m.Resource)
			}
		case m.Skipped != nil:
			inv.Skipped = append(inv.Skipped, *m.Skipped)
		case m.Warning != nil:
			inv.Warnings = append(inv.Warnings, *m.Warning)
		}
		return nil
	})
	return inv, err
}
