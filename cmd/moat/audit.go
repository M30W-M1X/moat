package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/M30W-M1X/moat/internal/audit"
	"github.com/M30W-M1X/moat/internal/moatapi"
)

// runAudit implements `moat audit {tail,query,report,verify}`.
func runAudit(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: moat audit {tail|query|report|verify} [flags]")
		return 2
	}
	sub, rest := args[0], args[1:]
	fs := flag.NewFlagSet("audit "+sub, flag.ContinueOnError)
	addr := fs.String("addr", os.Getenv("MOAT_ADDR"), "moatd address (host:port or URL)")
	token := fs.String("token", os.Getenv("MOAT_TOKEN"), "bearer token")
	dir := fs.String("dir", "", "read the audit files directly instead of the API")
	asJSON := fs.Bool("json", false, "emit JSON")
	sandbox := fs.String("sandbox", "", "filter by sandbox id")
	ftoken := fs.String("filter-token", "", "filter by token id (needs audit_read=all)")
	typ := fs.String("type", "", "filter by event type")
	since := fs.String("since", "", "start time (RFC3339 or -15m)")
	until := fs.String("until", "", "end time (RFC3339 or -15m)")
	limit := fs.Int("limit", 200, "max events")
	follow := fs.Bool("f", false, "tail: keep polling for new events")
	if err := fs.Parse(rest); err != nil {
		return 2
	}

	ctx, cancel := signalContext()
	defer cancel()

	var types []string
	if *typ != "" {
		types = []string{*typ}
	}

	switch sub {
	case "verify":
		return auditVerify(ctx, *dir, *addr, *token, *asJSON)
	case "report":
		if fs.NArg() < 1 {
			fmt.Fprintln(os.Stderr, "usage: moat audit report <sandbox-id>")
			return 2
		}
		return auditReport(ctx, fs.Arg(0), *dir, *addr, *token, *asJSON)
	case "query", "tail":
		f := auditFilter{sandbox: *sandbox, token: *ftoken, types: types,
			since: *since, until: *until, limit: *limit, desc: sub == "tail"}
		return auditList(ctx, f, *dir, *addr, *token, *asJSON, *follow && sub == "tail")
	default:
		fmt.Fprintf(os.Stderr, "unknown audit subcommand %q\n", sub)
		return 2
	}
}

type auditFilter struct {
	sandbox, token string
	types          []string
	since, until   string
	limit          int
	desc           bool
}

func auditVerify(ctx context.Context, dir, addr, token string, asJSON bool) int {
	if dir != "" {
		res, err := audit.Verify(dir)
		if err != nil {
			fmt.Fprintln(os.Stderr, "verify:", err)
			return 1
		}
		printVerify(res, asJSON)
		if res.OK {
			return 0
		}
		return 1
	}
	if addr == "" || token == "" {
		fmt.Fprintln(os.Stderr, "verify over the API needs -addr and -token, or use -dir")
		return 2
	}
	// The API has no verify endpoint. verify runs client-side over a full
	// pull. An unfiltered pull needs a token with audit_read=all.
	c := moatapi.New(addr, token)
	evs, err := c.Audit(ctx, moatapi.AuditFilter{Limit: 100000})
	if err != nil {
		fmt.Fprintln(os.Stderr, "verify:", err)
		return 1
	}
	res := verifyEvents(evs)
	printVerify(res, asJSON)
	if res.OK {
		return 0
	}
	return 1
}

func auditList(ctx context.Context, f auditFilter, dir, addr, token string, asJSON, follow bool) int {
	emit := func(evs []audit.Event) {
		w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
		for _, e := range evs {
			if asJSON {
				b, _ := json.Marshal(auditWireCLI(e))
				fmt.Println(string(b))
				continue
			}
			fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\t%v\n",
				e.Seq, e.TS.Format(time.RFC3339), e.Type, e.Sandbox, e.Token, e.Attrs)
		}
		w.Flush()
	}
	if dir != "" {
		fil := audit.Filter{Sandbox: f.sandbox, Token: f.token,
			Types: f.types, Limit: f.limit, Desc: f.desc}
		if f.since != "" {
			if ts, ok := audit.ParseWhen(f.since); ok {
				fil.Since = ts
			}
		}
		if f.until != "" {
			if ts, ok := audit.ParseWhen(f.until); ok {
				fil.Until = ts
			}
		}
		evs, err := audit.Query(dir, fil)
		if err != nil {
			fmt.Fprintln(os.Stderr, "query:", err)
			return 1
		}
		emit(evs)
		return 0
	}
	if addr == "" || token == "" {
		fmt.Fprintln(os.Stderr, "need -addr and -token, or -dir")
		return 2
	}
	c := moatapi.New(addr, token)
	pull := func() ([]audit.Event, error) {
		me, err := c.Audit(ctx, moatapi.AuditFilter{Sandbox: f.sandbox, Token: f.token,
			Types: f.types, Since: f.since, Until: f.until, Limit: f.limit, Desc: f.desc})
		if err != nil {
			return nil, err
		}
		return fromWire(me), nil
	}
	evs, err := pull()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	emit(evs)
	if !follow {
		return 0
	}
	var lastSeq uint64
	for _, e := range evs {
		if e.Seq > lastSeq {
			lastSeq = e.Seq
		}
	}
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return 0
		case <-t.C:
			more, err := pull()
			if err != nil {
				continue
			}
			var fresh []audit.Event
			for _, e := range more {
				if e.Seq > lastSeq {
					fresh = append(fresh, e)
					lastSeq = e.Seq
				}
			}
			if len(fresh) > 0 {
				emit(fresh)
			}
		}
	}
}

func auditReport(ctx context.Context, id, dir, addr, token string, asJSON bool) int {
	var rep any
	if dir != "" {
		evs, err := audit.Query(dir, audit.Filter{Sandbox: id, Limit: 100000})
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		if len(evs) == 0 {
			fmt.Fprintln(os.Stderr, "no events for", id)
			return 1
		}
		rep = audit.BuildReport(evs)
	} else {
		if addr == "" || token == "" {
			fmt.Fprintln(os.Stderr, "need -addr and -token, or -dir")
			return 2
		}
		r, err := moatapi.New(addr, token).Report(ctx, id)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		rep = r
	}
	b, _ := json.MarshalIndent(rep, "", "  ")
	fmt.Println(string(b))
	return 0
}

func printVerify(res audit.VerifyResult, asJSON bool) {
	if asJSON {
		b, _ := json.MarshalIndent(res, "", "  ")
		fmt.Println(string(b))
		return
	}
	status := "chain OK"
	if !res.OK {
		status = "CHAIN BROKEN"
	}
	fmt.Printf("%s  %d events  seq %d..%d\n", status, res.Events, res.FirstSeq, res.LastSeq)
	if res.PrunedPrefix {
		fmt.Printf("  chain starts at seq %d (earlier entries pruned)\n", res.FirstSeq)
	}
	for _, p := range res.Problems {
		fmt.Println("  !", p)
	}
}

func auditWireCLI(e audit.Event) map[string]any {
	return map[string]any{"seq": e.Seq, "ts": e.TS.UTC().Format(time.RFC3339Nano),
		"type": e.Type, "sandbox": e.Sandbox, "token": e.Token, "attrs": e.Attrs,
		"prev": e.Prev, "hash": e.Hash}
}

func fromWire(me []moatapi.AuditEvent) []audit.Event {
	out := make([]audit.Event, len(me))
	for i, e := range me {
		out[i] = audit.Event{Seq: e.Seq, TS: e.TS, Type: e.Type, Sandbox: e.Sandbox,
			Token: e.Token, Attrs: e.Attrs, Prev: e.Prev, Hash: e.Hash}
	}
	return out
}

func verifyEvents(me []moatapi.AuditEvent) audit.VerifyResult {
	return audit.VerifyEvents(fromWire(me))
}
