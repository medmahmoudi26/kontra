package e2e

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
	"go.temporal.io/api/common/v1"
	"go.temporal.io/api/enums/v1"
	"go.temporal.io/api/temporalproto"
	"go.temporal.io/api/workflowservice/v1"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// The marker scan of sealed_no_plaintext (ext-sealed.md §9, D171; M3 plan, e2e harness): a random marker carried
// in every input, answer and output of a sealed task must be absent, in raw, JSON-escaped, hex, base64 and base64url
// forms, from every table of schema kontra (each row cast to text), both replicas' logs (at debug level), every
// node's log, the Temporal histories of the run's workflows (payloads decoded: the history JSON carries them in
// base64) and every object of the bucket.

// NewMarker is a random marker. It holds '<' and '>', which JSON encoders write escaped (\u003c), so the escaped
// form differs from the raw one and both are looked for.
func NewMarker() string { return "mk<" + randomHex(10) + ">" }

// MarkerForms are the needles a marker is looked for as: raw, JSON-escaped (with and without HTML escaping), hex,
// and in base64 and base64url at each of the three alignments a byte string can start at (the characters that
// depend on neighbouring bytes cut off).
func MarkerForms(marker string) []string {
	m := []byte(marker)
	forms := []string{marker, hex.EncodeToString(m)}
	if j, err := json.Marshal(marker); err == nil {
		forms = append(forms, string(j[1:len(j)-1]))
	}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if enc.Encode(marker) == nil {
		s := strings.TrimSpace(b.String())
		forms = append(forms, s[1:len(s)-1])
	}
	for _, e := range []*base64.Encoding{base64.StdEncoding, base64.URLEncoding} {
		for k := 0; k < 3; k++ {
			s := strings.TrimRight(e.EncodeToString(append(make([]byte, k), m...)), "=")
			skip := []int{0, 2, 3}[k] // the characters that hold bits of the k bytes before the marker
			forms = append(forms, s[skip:len(s)-3])
		}
	}
	slices.Sort(forms)
	return slices.Compact(forms)
}

// Finding is where a marker was found.
type Finding struct {
	Where string // "table kontra.tasks", "log gate-A.log", "history <workflow id>", "bucket <key>"
	Form  string
}

func (f Finding) String() string { return f.Where + " (" + f.Form + ")" }

// ScanMarker looks for every form of marker in the run's tables, logs, histories and bucket, and returns where it
// is found (nothing: the marker is absent).
func (h *Harness) ScanMarker(ctx context.Context, marker string) ([]Finding, error) {
	forms := MarkerForms(marker)
	var out []Finding
	look := func(where string, data []byte) {
		for _, f := range forms {
			if bytes.Contains(data, []byte(f)) {
				out = append(out, Finding{Where: where, Form: f})
			}
		}
	}
	// Every table of schema kontra, each row as text.
	tables, err := h.tables(ctx)
	if err != nil {
		return nil, err
	}
	for _, t := range tables {
		rows, err := h.Pool.Query(ctx, `select x::text from kontra.`+pgx.Identifier{t}.Sanitize()+` x`)
		if err != nil {
			return nil, fmt.Errorf("e2e: scan kontra.%s: %w", t, err)
		}
		texts, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return nil, fmt.Errorf("e2e: scan kontra.%s: %w", t, err)
		}
		look("table kontra."+t, []byte(strings.Join(texts, "\n")))
	}
	// Both replicas' logs, every node's.
	for _, g := range h.Gates {
		if g != nil {
			b, err := os.ReadFile(g.LogPath)
			if err != nil {
				return nil, err
			}
			look("log of gate "+g.Name, b)
		}
	}
	h.mu.Lock()
	nodes := slices.Clone(h.nodes)
	h.mu.Unlock()
	for _, n := range nodes {
		if b, err := os.ReadFile(n.LogPath); err == nil {
			look("log of node "+n.Name, b)
		}
	}
	// The Temporal histories.
	hist, err := h.Histories(ctx)
	if err != nil {
		return nil, err
	}
	for id, text := range hist {
		look("history "+id, text)
	}
	// The bucket.
	if h.Bucket != nil {
		objs, err := h.Bucket.Objects()
		if err != nil {
			return nil, err
		}
		for k, b := range objs {
			look("bucket "+k, b)
		}
	}
	return out, nil
}

// tables are the tables of schema kontra.
func (h *Harness) tables(ctx context.Context) ([]string, error) {
	rows, err := h.Pool.Query(ctx, `select table_name from information_schema.tables
		where table_schema = 'kontra' and table_type = 'BASE TABLE' order by table_name`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// Histories returns the history of every workflow of the run's Temporal server (one server per test), as JSON
// followed by the bytes of every payload in it, by workflow id.
func (h *Harness) Histories(ctx context.Context) (map[string][]byte, error) {
	out := map[string][]byte{}
	var token []byte
	for {
		resp, err := h.TC.ListWorkflow(ctx, &workflowservice.ListWorkflowExecutionsRequest{Namespace: Namespace,
			PageSize: 100, NextPageToken: token})
		if err != nil {
			return nil, fmt.Errorf("e2e: list workflows: %w", err)
		}
		for _, ex := range resp.Executions {
			id, runID := ex.GetExecution().GetWorkflowId(), ex.GetExecution().GetRunId()
			it := h.TC.GetWorkflowHistory(ctx, id, runID, false, enums.HISTORY_EVENT_FILTER_TYPE_ALL_EVENT)
			var b bytes.Buffer
			for it.HasNext() {
				ev, err := it.Next()
				if err != nil {
					return nil, fmt.Errorf("e2e: history of %s: %w", id, err)
				}
				js, err := temporalproto.CustomJSONMarshalOptions{}.Marshal(ev)
				if err != nil {
					return nil, err
				}
				b.Write(js)
				payloadData(ev.ProtoReflect(), &b)
			}
			out[id+"/"+runID] = b.Bytes()
		}
		token = resp.NextPageToken
		if len(token) == 0 {
			return out, nil
		}
	}
}

// payloadData writes the data of every temporal.api.common.v1.Payload inside m to b.
func payloadData(m protoreflect.Message, b *bytes.Buffer) {
	if p, ok := m.Interface().(*common.Payload); ok {
		b.WriteByte('\n')
		b.Write(p.GetData())
		return
	}
	m.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		switch {
		case fd.IsList():
			if fd.Message() != nil {
				for i, l := 0, v.List(); i < l.Len(); i++ {
					payloadData(l.Get(i).Message(), b)
				}
			}
		case fd.IsMap():
			if fd.MapValue().Message() != nil {
				v.Map().Range(func(_ protoreflect.MapKey, mv protoreflect.Value) bool {
					payloadData(mv.Message(), b)
					return true
				})
			}
		case fd.Message() != nil:
			payloadData(v.Message(), b)
		}
		return true
	})
}
