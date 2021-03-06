package exdgo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"
)

// ReplayRequestParam is the parameters to make new `ReplayRequest`.
type ReplayRequestParam struct {
	// Map of exchanges and and its channels to filter-in.
	Filter map[string][]string
	// Start date-time.
	Start time.Time
	// End date-time.
	End time.Time
}

// ReplayRequest replays market data.
//
// There is two ways of reading the response:
// - `download` to immidiately start downloading the whole response as one array.
// - `stream` to return iterable object yields line by line.
type ReplayRequest struct {
	cli    *Client
	filter map[string][]string
	start  int64
	end    int64
}

func setupReplayRequest(cli *Client, param ReplayRequestParam) (*ReplayRequest, error) {
	req := new(ReplayRequest)
	req.cli = cli
	var serr error
	req.filter, serr = copyFilter(param.Filter)
	if serr != nil {
		return nil, serr
	}
	start := param.Start.UnixNano()
	end := param.End.UnixNano()
	if start >= end {
		return nil, errors.New("'Start' >= 'End'")
	}
	req.start = start
	req.end = end
	return req, nil
}

type rawLineProcessor struct {
	// map[exchange]map[channel]map[field]type
	defs map[string]map[string]map[string]string
}

func newRawLineProcessor() *rawLineProcessor {
	p := new(rawLineProcessor)
	p.defs = make(map[string]map[string]map[string]string)
	return p
}

func (p *rawLineProcessor) processRawLine(line *StringLine) (ret StructLine, ok bool, err error) {
	if line.Type == LineTypeStart {
		// Delete definition
		delete(p.defs, line.Exchange)
	}
	if line.Type != LineTypeMessage {
		ret = StructLine{
			Exchange:  line.Exchange,
			Type:      line.Type,
			Timestamp: line.Timestamp,
			Channel:   line.Channel,
			Message:   line.Message,
		}
		ok = true
		return
	}

	exchange := line.Exchange
	// Channel and message fields are always available since Type == LineTypeMsg
	channel := *line.Channel
	message := line.Message

	if _, sok := p.defs[exchange]; !sok {
		// This is the first line for this exchange
		p.defs[exchange] = make(map[string]map[string]string)
	}
	def, sok := p.defs[exchange][channel]
	if !sok {
		def = make(map[string]string)
		if serr := json.Unmarshal(message, &def); serr != nil {
			err = fmt.Errorf("def update unmarshal: %v", serr)
			return
		}
		p.defs[exchange][channel] = def
		return
	}
	msgObj := make(map[string]interface{})
	serr := json.Unmarshal(message, &msgObj)
	if serr != nil {
		err = fmt.Errorf("message unmarshal: %v", serr)
		return
	}

	// Type conversion according to the received definition
	for name, typ := range def {
		if val, sok := msgObj[name]; sok && val != nil {
			if typ == "timestamp" || typ == "duration" {
				msgObj[name], serr = strconv.ParseInt(val.(string), 10, 64)
				if serr != nil {
					err = fmt.Errorf("type conversion: %v", serr)
					return
				}
			} else if typ == "int" {
				msgObj[name] = int64(val.(float64))
			}
		}
	}

	ret = StructLine{
		Exchange:   exchange,
		Type:       line.Type,
		Timestamp:  line.Timestamp,
		Channel:    line.Channel,
		Message:    msgObj,
		Definition: def,
	}
	ok = true
	return
}

// DownloadWithContext is same as `Download()`, but sends requests in given concurrency
// in given context.
func (r *ReplayRequest) DownloadWithContext(ctx context.Context, concurrency int) ([]StructLine, error) {
	format := "json"
	rawReq := RawRequest{
		cli:    r.cli,
		filter: r.filter,
		start:  r.start,
		end:    r.end,
		format: &format,
	}
	slice, serr := rawReq.DownloadWithContext(ctx, concurrency)
	if serr != nil {
		return nil, serr
	}
	result := make([]StructLine, 0, len(slice))
	processor := newRawLineProcessor()
	for i := range slice {
		processed, ok, serr := processor.processRawLine(&slice[i])
		if !ok {
			if serr != nil {
				return nil, serr
			}
			continue
		}
		result = append(result, processed)
	}
	return result, nil
}

// DownloadConcurrency is same as `Download()`, but sends requests in given concurrency.
func (r *ReplayRequest) DownloadConcurrency(concurrency int) ([]StructLine, error) {
	return r.DownloadWithContext(context.Background(), concurrency)
}

// Download sends request and download response in an slice.
// Returns slice if and only if an error was not reported.
// Otherwise, slice is non-nil.
func (r *ReplayRequest) Download() ([]StructLine, error) {
	return r.DownloadWithContext(context.Background(), downloadBatchSize)
}

type replayStreamIterator struct {
	req       *ReplayRequest
	rawItr    StringLineIterator
	processor *rawLineProcessor
}

func newReplayStreamIterator(ctx context.Context, req *ReplayRequest, bufferSize int) (*replayStreamIterator, error) {
	i := new(replayStreamIterator)
	format := "json"
	rawRequest := RawRequest{
		cli:    req.cli,
		filter: req.filter,
		start:  req.start,
		end:    req.end,
		format: &format,
	}
	itr, serr := rawRequest.StreamWithContext(ctx, bufferSize)
	if serr != nil {
		return nil, serr
	}
	i.rawItr = itr
	i.processor = newRawLineProcessor()
	return i, nil
}

func (i *replayStreamIterator) Next() (*StructLine, bool, error) {
	for {
		line, ok, serr := i.rawItr.Next()
		if !ok {
			if serr != nil {
				return nil, false, serr
			}
			// No more lines
			return nil, false, nil
		}
		processed, ok, serr := i.processor.processRawLine(line)
		if !ok {
			if serr != nil {
				return nil, false, serr
			}
			continue
		}
		return &processed, true, nil
	}
}

func (i *replayStreamIterator) Close() error {
	serr := i.rawItr.Close()
	if serr != nil {
		return serr
	}
	return nil
}

// StructLineIterator is the interface of iterator which yields `*StructLine`.
type StructLineIterator interface {
	// Next returns the next line from the iterator.
	// If the next line exists, `ok` is true ad `line` is non-nil, otherwise false and `line` is nil.
	// `ok` is false if an error was returned.
	Next() (line *StructLine, ok bool, err error)

	// Close frees resources this iterator is using.
	// **Must** always be called after the use of this iterator.
	Close() error
}

// Stream sends requests to the server and returns an iterator for reading the response.
//
// Returns an iterator yields line by line if and only if an error is not returned.
// The iterator yields immidiately if a line is bufferred, waits for download if not avaliable.
//
// Lines will be bufferred immidiately after calling this function.
//
// Higher responsiveness than `download` is expected as it does not have to wait for
// the entire data to be downloaded.
func (r *ReplayRequest) Stream() (StructLineIterator, error) {
	return r.StreamWithContext(context.Background(), defaultBufferSize)
}

// StreamBufferSize is same as Stream but with custom bufferSize.
// `bufferSize` is the desired buffer size to store streaming data.
// One shard is equavalent to one minute.
func (r *ReplayRequest) StreamBufferSize(bufferSize int) (StructLineIterator, error) {
	return r.StreamWithContext(context.Background(), bufferSize)
}

// StreamWithContext is same as `Stream` but a context can be given.
// Background downloads and the returned iterator will use the context for their lifetime.
// Cancelling the context will stop running background downloads, and future `next` calls to the iterator might produce error.
func (r *ReplayRequest) StreamWithContext(ctx context.Context, bufferSize int) (StructLineIterator, error) {
	itr, serr := newReplayStreamIterator(ctx, r, bufferSize)
	if serr != nil {
		return nil, serr
	}
	return itr, nil
}

// Replay creates new `ReplayRequest` with the given parameters and returns its pointer.
// Return is nil if an error was returned.
func Replay(clientParam ClientParam, param ReplayRequestParam) (*ReplayRequest, error) {
	cliSetting, serr := setupClient(clientParam)
	if serr != nil {
		return nil, serr
	}
	return setupReplayRequest(&cliSetting, param)
}

// Replay creates `RawRequest` and return its pointer if and only if an error was not returned.
// Otherwise, it's nil.
func (c *Client) Replay(param ReplayRequestParam) (*ReplayRequest, error) {
	return setupReplayRequest(c, param)
}
