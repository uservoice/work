package discard

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/uservoice/work"
)

func TestInvalidPayload(t *testing.T) {
	job := work.NewJob()
	opt := &work.DequeueOptions{
		Namespace: "n1",
		QueueID:   "q1",
	}
	h := InvalidPayload(func(*work.Job, *work.DequeueOptions) error {
		return errors.New("msgpack: decode")
	})

	err := h(job, opt)
	require.Error(t, err)
	require.Equal(t, work.ErrUnrecoverable, err)
}
