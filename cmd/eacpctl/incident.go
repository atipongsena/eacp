package main

import (
	"context"
	"errors"
	"io"
	"net/url"
	"strconv"

	"github.com/google/uuid"
)

const incidentUsage = `usage: eacpctl incident list [--state S] [--severity S] [--kind K] [--limit N]
  | show <id> | open --title T --severity S --reason R [--subject-type K --subject-id <uuid>]
  | ack <id> --reason R | assign <id> <principal-uuid|none> | note <id> --text T
  | link <id> <kind> <uuid> | resolve <id> --code <contained|false_positive|accepted_risk|duplicate> --reason R`

const socUsage = "usage: eacpctl soc summary"

func runSOC(ctx context.Context, args []string, getenv func(string) string, out io.Writer) error {
	if len(args) != 1 || args[0] != "summary" {
		return errors.New(socUsage)
	}
	return call(ctx, getenv, out, "GET", "/v1/soc/summary", nil)
}

func runIncident(ctx context.Context, args []string, getenv func(string) string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New(incidentUsage)
	}
	switch args[0] {
	case "list":
		fs := newFlags("incident list")
		state := fs.String("state", "", "OPEN, ACKNOWLEDGED or RESOLVED")
		severity := fs.String("severity", "", "low, medium, high or critical")
		kind := fs.String("kind", "", "incident kind")
		limit := fs.Int("limit", 0, "at most 500")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if len(fs.Args()) != 0 {
			return errors.New(incidentUsage)
		}
		q := url.Values{}
		for k, v := range map[string]string{"state": *state, "severity": *severity, "kind": *kind} {
			if v != "" {
				q.Set(k, v)
			}
		}
		if *limit != 0 {
			q.Set("limit", strconv.Itoa(*limit))
		}
		path := "/v1/incidents"
		if enc := q.Encode(); enc != "" {
			path += "?" + enc
		}
		return call(ctx, getenv, out, "GET", path, nil)
	case "open":
		fs := newFlags("incident open")
		title := fs.String("title", "", "what is wrong")
		severity := fs.String("severity", "", "low, medium, high or critical")
		reason := fs.String("reason", "", "why it is an incident")
		subjectType := fs.String("subject-type", "", "tool, connector, agent, agent_version, action, …")
		subjectID := fs.String("subject-id", "", "the subject's uuid")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if *title == "" || *severity == "" || *reason == "" || len(fs.Args()) != 0 ||
			(*subjectType == "") != (*subjectID == "") {
			return errors.New(incidentUsage)
		}
		body := map[string]any{"title": *title, "severity": *severity, "reason": *reason}
		if *subjectType != "" {
			id, err := uuid.Parse(*subjectID)
			if err != nil {
				return errors.New(incidentUsage)
			}
			body["subject_type"], body["subject_id"] = *subjectType, id
		}
		return call(ctx, getenv, out, "POST", "/v1/incidents", body)
	}
	if len(args) < 2 {
		return errors.New(incidentUsage)
	}
	id, err := uuid.Parse(args[1])
	if err != nil {
		return errors.New(incidentUsage)
	}
	path := "/v1/incidents/" + id.String()
	switch args[0] {
	case "show":
		if len(args) != 2 {
			return errors.New(incidentUsage)
		}
		return call(ctx, getenv, out, "GET", path, nil)
	case "assign":
		if len(args) != 3 {
			return errors.New(incidentUsage)
		}
		var assignee any
		if args[2] != "none" {
			p, err := uuid.Parse(args[2])
			if err != nil {
				return errors.New(incidentUsage)
			}
			assignee = p
		}
		return call(ctx, getenv, out, "POST", path+"/assign", map[string]any{"assignee_id": assignee})
	case "link":
		if len(args) != 4 {
			return errors.New(incidentUsage)
		}
		target, err := uuid.Parse(args[3])
		if err != nil {
			return errors.New(incidentUsage)
		}
		return call(ctx, getenv, out, "POST", path+"/links", map[string]any{"kind": args[2], "id": target})
	case "ack", "note", "resolve":
		fs := newFlags("incident " + args[0])
		reason := fs.String("reason", "", "reason")
		text := fs.String("text", "", "note text")
		code := fs.String("code", "", "resolution code")
		if err := fs.Parse(args[2:]); err != nil {
			return err
		}
		if len(fs.Args()) != 0 {
			return errors.New(incidentUsage)
		}
		switch {
		case args[0] == "ack" && *reason != "":
			return call(ctx, getenv, out, "POST", path+"/acknowledge", map[string]any{"reason": *reason})
		case args[0] == "note" && *text != "":
			return call(ctx, getenv, out, "POST", path+"/notes", map[string]any{"text": *text})
		case args[0] == "resolve" && *code != "" && *reason != "":
			return call(ctx, getenv, out, "POST", path+"/resolve", map[string]any{"resolution": *code, "reason": *reason})
		}
	}
	return errors.New(incidentUsage)
}
