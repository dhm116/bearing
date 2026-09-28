package adapter

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"sync"

	"bearing.example/pkg/model"
)

// Client talks to one adapter over the protocol. It is safe for concurrent
// use; calls are serialized because the protocol handles one request at a time.
type Client struct {
	mu     sync.Mutex
	enc    *json.Encoder
	sc     *bufio.Scanner
	nextID int
	close  func() error
}

// NewClient speaks the protocol over an existing reader and writer, such as
// in-memory pipes in tests.
func NewClient(r io.Reader, w io.Writer) *Client {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), maxMessage)
	return &Client{enc: json.NewEncoder(w), sc: sc, close: func() error { return nil }}
}

// Start launches an adapter executable and connects to its stdin and stdout.
// The adapter's stderr is passed through to this process's stderr.
func Start(ctx context.Context, name string, args ...string) (*Client, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stderr = os.Stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start adapter %s: %w", name, err)
	}
	c := NewClient(stdout, stdin)
	c.close = func() error {
		stdin.Close()
		return cmd.Wait()
	}
	return c, nil
}

// Close ends the session and, for started adapters, waits for the process.
func (c *Client) Close() error { return c.close() }

func (c *Client) call(method string, params, result any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.nextID++
	id := json.RawMessage(strconv.Itoa(c.nextID))
	var raw json.RawMessage
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			return err
		}
		raw = b
	}
	if err := c.enc.Encode(request{JSONRPC: "2.0", ID: id, Method: method, Params: raw}); err != nil {
		return fmt.Errorf("%s: write: %w", method, err)
	}
	if !c.sc.Scan() {
		if err := c.sc.Err(); err != nil {
			return fmt.Errorf("%s: read: %w", method, err)
		}
		return fmt.Errorf("%s: adapter closed the connection", method)
	}
	var resp struct {
		ID     json.RawMessage `json:"id"`
		Result json.RawMessage `json:"result"`
		Error  *Error          `json:"error"`
	}
	if err := json.Unmarshal(c.sc.Bytes(), &resp); err != nil {
		return fmt.Errorf("%s: decode response: %w", method, err)
	}
	if string(resp.ID) != string(id) {
		return fmt.Errorf("%s: response id %s does not match request id %s", method, resp.ID, id)
	}
	if resp.Error != nil {
		return resp.Error
	}
	return json.Unmarshal(resp.Result, result)
}

// Describe calls bearing.describe.
func (c *Client) Describe(ctx context.Context) (DescribeResult, error) {
	var res DescribeResult
	err := c.call(MethodDescribe, nil, &res)
	return res, err
}

// Sync calls bearing.sync for one page.
func (c *Client) Sync(ctx context.Context, p SyncParams) (SyncResult, error) {
	var res SyncResult
	err := c.call(MethodSync, p, &res)
	return res, err
}

// Handle calls bearing.handle.
func (c *Client) Handle(ctx context.Context, p HandleParams) (HandleResult, error) {
	var res HandleResult
	err := c.call(MethodHandle, p, &res)
	return res, err
}

// ErrTooManyPages is returned by SyncAll when an adapter never reports done.
var ErrTooManyPages = errors.New("adapter returned too many pages without finishing")

// SyncAll pages through a full sync, validating every observation and passing
// it to emit. maxPages guards against adapters that never finish.
func SyncAll(ctx context.Context, a interface {
	Sync(context.Context, SyncParams) (SyncResult, error)
}, config json.RawMessage, maxPages int, emit func(model.Observation) error) error {
	cursor := ""
	for page := 0; page < maxPages; page++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		res, err := a.Sync(ctx, SyncParams{Config: config, Cursor: cursor})
		if err != nil {
			return fmt.Errorf("sync page %d: %w", page+1, err)
		}
		for _, o := range res.Observations {
			if err := o.Validate(); err != nil {
				return fmt.Errorf("sync page %d: %w", page+1, err)
			}
			if err := emit(o); err != nil {
				return err
			}
		}
		if res.Done {
			return nil
		}
		if res.NextCursor == "" || res.NextCursor == cursor {
			return fmt.Errorf("sync page %d: adapter is not done but returned no new cursor", page+1)
		}
		cursor = res.NextCursor
	}
	return ErrTooManyPages
}
