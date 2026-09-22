package azure

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"
)

// maxPolls bounds a poll chain the way maxPages bounds a page chain. The real
// bound is the caller's context; this one only stops a server that answers
// InProgress forever from spinning a goroutine for the life of the hub.
const maxPolls = 600

// pollFloor and pollCeiling frame the wait when Azure sends no Retry-After.
// Below the floor the hub hammers an operation that takes minutes; above the
// ceiling a VM that came up in twenty seconds is reported a minute late.
const (
	pollFloor   = time.Second
	pollCeiling = 30 * time.Second
)

// OperationError is an asynchronous operation that Azure ran and refused, as
// opposed to a request Azure rejected. Deliberately not an *Error: the poll
// that carried this news answered 200, so StatusOf must not turn a failed VM
// creation into an HTTP success.
type OperationError struct {
	Status  string // Azure's own terminal status: Failed or Canceled
	Code    string // Azure's own error code, when it sent one
	Message string
}

func (e *OperationError) Error() string {
	switch {
	case e.Code != "" && e.Message != "":
		return fmt.Sprintf("azure operation %s (%s): %s", e.Status, e.Code, truncate(e.Message, 400))
	case e.Message != "":
		return fmt.Sprintf("azure operation %s: %s", e.Status, truncate(e.Message, 400))
	default:
		return "azure operation " + e.Status
	}
}

// Await follows an accepted request to its terminal state.
//
// It returns nil when the operation succeeded — including when there was
// nothing to follow. It returns an *OperationError when Azure ran the
// operation and reported a failure, and the context's error when the
// caller's budget ran out, which is not a failure of the operation and must
// never be recorded as one.
//
// "Nothing to follow" is narrower than "not a 202". Compute answers a VM PUT
// with 201 Created while the machine is still being built, and says so with
// an Azure-AsyncOperation header; the first real VM was recorded as
// succeeded two seconds after the request because that 201 was taken as
// finished. Any 2xx that carries the header is followed. Only a 202 falls
// back to Location: on a 201 that header, when present, names the resource,
// not an operation.
func Await(ctx context.Context, c *Client, r Response) error {
	if r.Status != http.StatusAccepted {
		if u := r.Header.Get("Azure-AsyncOperation"); u != "" && r.Status >= 200 && r.Status < 300 {
			return poll(ctx, c, u, r.Header.Get("Retry-After"), operationStatus)
		}
		return nil
	}
	// Azure sends both headers on a create. Only Azure-AsyncOperation carries
	// a status, and it is authoritative; Location merely stops answering 202
	// once the *result* exists, which for a create is earlier than the machine
	// being ready. They are not two spellings of one thing.
	if u := r.Header.Get("Azure-AsyncOperation"); u != "" {
		return poll(ctx, c, u, r.Header.Get("Retry-After"), operationStatus)
	}
	if u := r.Header.Get("Location"); u != "" {
		return poll(ctx, c, u, r.Header.Get("Retry-After"), locationStatus)
	}
	return errors.New("azure: the request was accepted but carried neither " +
		"an Azure-AsyncOperation nor a Location header, so there is no way to learn how it ended")
}

// terminal reports whether a poll answer ends the wait, and how.
type terminal func(Response) (done bool, err error)

func poll(ctx context.Context, c *Client, u, firstWait string, done terminal) error {
	wait := pollWait(firstWait, 1)
	for n := 1; n <= maxPolls; n++ {
		if !c.opt.Sleep(ctx, wait) {
			return ctx.Err()
		}
		resp, err := c.GetResponse(ctx, u)
		if err != nil {
			return err
		}
		finished, err := done(resp)
		if finished || err != nil {
			return err
		}
		wait = pollWait(resp.Header.Get("Retry-After"), n+1)
	}
	return fmt.Errorf("azure: operation still running after %d polls, giving up on following it", maxPolls)
}

// pollWait prefers Azure's own pacing and otherwise doubles, framed.
func pollWait(hdr string, n int) time.Duration {
	if d := retryAfter(hdr); d > 0 {
		return d
	}
	w := pollFloor
	for i := 1; i < n && w < pollCeiling; i++ {
		w *= 2
	}
	if w > pollCeiling {
		return pollCeiling
	}
	return w
}

// operationStatus reads the Azure-AsyncOperation body, whose `status` is the
// authority on how the operation ended.
func operationStatus(r Response) (bool, error) {
	var body struct {
		Status string `json:"status"`
		Error  struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(r.Body, &body); err != nil {
		return false, fmt.Errorf("azure: operation status is not an object: %w", err)
	}
	switch body.Status {
	case "Succeeded":
		return true, nil
	case "Failed", "Canceled":
		return true, &OperationError{Status: body.Status, Code: body.Error.Code, Message: body.Error.Message}
	case "":
		return false, errors.New("azure: the operation status endpoint answered without a status field")
	default: // InProgress, Accepted, Running, or anything Azure adds later
		return false, nil
	}
}

// locationStatus is the fallback, and it works differently: there is no status
// to read. The operation is over when the URL stops answering 202.
func locationStatus(r Response) (bool, error) {
	return r.Status != http.StatusAccepted, nil
}
