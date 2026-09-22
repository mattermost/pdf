// Copyright 2014 The Go Authors.  All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package pdf

import (
	"context"
	"fmt"
	"io"
	"strings"
)

// A Stack represents a stack of values.
type Stack struct {
	stack []Value
}

func (stk *Stack) Len() int {
	return len(stk.stack)
}

func (stk *Stack) Push(v Value) {
	stk.stack = append(stk.stack, v)
}

func (stk *Stack) Pop() Value {
	n := len(stk.stack)
	if n == 0 {
		return Value{}
	}
	v := stk.stack[n-1]
	stk.stack[n-1] = Value{}
	stk.stack = stk.stack[:n-1]
	return v
}

func newDict() Value {
	return Value{nil, objptr{}, make(dict)}
}

// Interpret interprets the content in a stream as a basic PostScript program,
// pushing values onto a stack and then calling the do function to execute
// operators. The do function may push or pop values from the stack as needed
// to implement op.
//
// Interpret handles the operators "dict", "currentdict", "begin", "end", "def", and "pop" itself.
//
// Interpret is not a full-blown PostScript interpreter. Its job is to handle the
// very limited PostScript found in certain supporting file formats embedded
// in PDF files, such as cmap files that describe the mapping from font code
// points to Unicode code points.
//
// A stream can also be represented by an array of streams; Interpret reads
// them as a single concatenated token stream, since operators and operands
// (e.g. array literals for the "TJ" operator) can be split across the
// individual streams.
//
// There is no support for executable blocks, among other limitations.
func Interpret(ctx context.Context, strm Value, do func(stk *Stack, op string)) error {
	var stk Stack
	var dicts []dict
	var rd io.Reader
	if strm.Kind() == Array {
		n := strm.Len()
		readers := make([]io.Reader, 0, max(2*n-1, 0))
		for i := 0; i < n; i++ {
			// strm.Index(i).Reader() initializes that stream's decode
			// filters immediately so check for cancellation before paying 
			// that cost instead of only after all of them are built.
			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
			}
			if i > 0 {
				// The PDF spec requires content streams in an array to be
				// treated as if concatenated with a space between each pair,
				// so a token can't be split across two streams (e.g. "10" at
				// the end of one and "20" at the start of the next must not
				// merge into "1020").
				readers = append(readers, strings.NewReader(" "))
			}
			readers = append(readers, strm.Index(i).Reader())
		}
		rd = io.MultiReader(readers...)
	} else {
		rd = strm.Reader()
	}

	b := newBuffer(rd, 0)
	b.ctx = ctx
	b.allowEOF = true
	b.allowObjptr = false
	b.allowStream = false

Reading:
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		tok := b.readToken()
		if tok == io.EOF {
			break
		}
		if kw, ok := tok.(keyword); ok {
			switch kw {
			case "null", "[", "]", "<<", ">>":
				break
			default:
				for i := len(dicts) - 1; i >= 0; i-- {
					if v, ok := dicts[i][name(kw)]; ok {
						stk.Push(Value{nil, objptr{}, v})
						continue Reading
					}
				}
				do(&stk, string(kw))
				continue
			case "dict":
				stk.Pop()
				stk.Push(Value{nil, objptr{}, make(dict)})
				continue
			case "currentdict":
				if len(dicts) == 0 {
					panic("no current dictionary")
				}
				stk.Push(Value{nil, objptr{}, dicts[len(dicts)-1]})
				continue
			case "begin":
				d := stk.Pop()
				if d.Kind() != Dict {
					panic("cannot begin non-dict")
				}
				dicts = append(dicts, d.data.(dict))
				continue
			case "end":
				if len(dicts) <= 0 {
					panic("mismatched begin/end")
				}
				dicts = dicts[:len(dicts)-1]
				continue
			case "def":
				if len(dicts) <= 0 {
					panic("def without open dict")
				}
				val := stk.Pop()
				key, ok := stk.Pop().data.(name)
				if !ok {
					// panic(fmt.Sprintf("def of non-name: %+v", stk.Pop().data))
					// Skip the value if it has key without value
					continue
				}
				dicts[len(dicts)-1][key] = val.data
				continue
			case "pop":
				stk.Pop()
				continue
			}
		}
		b.unreadToken(tok)
		obj := b.readObject()
		stk.Push(Value{nil, objptr{}, obj})
	}
	return nil
}

type seqReader struct {
	rd     io.Reader
	offset int64
}

func (r *seqReader) ReadAt(buf []byte, offset int64) (int, error) {
	if offset != r.offset {
		return 0, fmt.Errorf("non-sequential read of stream")
	}
	n, err := io.ReadFull(r.rd, buf)
	r.offset += int64(n)
	return n, err
}
