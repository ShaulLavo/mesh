package mcpnative

import (
	"context"
	"encoding/json"
)

const (
	defaultServiceMS = 30000
	maxServiceMS     = 120000
	maxServiceList   = 32
)

func parseServiceRouteCall(controls Controls, raw json.RawMessage, defaultMS, maxMS int) (ServiceRouteRequest, error) {
	if err := requireControls(controls); err != nil {
		return ServiceRouteRequest{}, err
	}
	var args struct {
		Host      string `json:"host"`
		Name      string `json:"name"`
		TimeoutMS *int   `json:"timeout_ms"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return ServiceRouteRequest{}, err
	}
	host, err := requireNonEmpty(args.Host, "host")
	if err != nil {
		return ServiceRouteRequest{}, err
	}
	name, err := requireNonEmpty(args.Name, "name")
	if err != nil {
		return ServiceRouteRequest{}, err
	}
	timeout, err := parseTimeout(args.TimeoutMS, defaultMS, maxMS)
	return ServiceRouteRequest{Host: host, Name: name, Timeout: timeout}, err
}

func handleServiceStart(ctx context.Context, controls Controls, raw json.RawMessage, _ func(ProgressUpdate)) (CallOutcome, error) {
	req, err := parseServiceRouteCall(controls, raw, defaultServiceMS, maxServiceMS)
	if err != nil {
		return CallOutcome{}, err
	}
	out, err := controls.StartService(ctx, req)
	return wrapControl("mesh_service_start", out, err)
}

func handleServiceStop(ctx context.Context, controls Controls, raw json.RawMessage, _ func(ProgressUpdate)) (CallOutcome, error) {
	req, err := parseServiceRouteCall(controls, raw, defaultOpMS, maxTimeoutMS)
	if err != nil {
		return CallOutcome{}, err
	}
	out, err := controls.StopService(ctx, req)
	return wrapControl("mesh_service_stop", out, err)
}

func handleServiceUnpublish(ctx context.Context, controls Controls, raw json.RawMessage, _ func(ProgressUpdate)) (CallOutcome, error) {
	req, err := parseServiceRouteCall(controls, raw, defaultOpMS, maxTimeoutMS)
	if err != nil {
		return CallOutcome{}, err
	}
	out, err := controls.UnpublishService(ctx, req)
	return wrapControl("mesh_service_unpublish", out, err)
}

func handleServicePublish(ctx context.Context, controls Controls, raw json.RawMessage, _ func(ProgressUpdate)) (CallOutcome, error) {
	if err := requireControls(controls); err != nil {
		return CallOutcome{}, err
	}
	req, err := parsePublishArgs(raw)
	if err != nil {
		return CallOutcome{}, err
	}
	out, err := controls.PublishService(ctx, req)
	return wrapControl("mesh_service_publish", out, err)
}

func parsePublishArgs(raw json.RawMessage) (PublishServiceRequest, error) {
	var args struct {
		Host           string   `json:"host"`
		Target         string   `json:"target"`
		Route          string   `json:"route"`
		Label          string   `json:"label"`
		Run            string   `json:"run"`
		Cwd            string   `json:"cwd"`
		Env            []string `json:"env"`
		Listen         []string `json:"listen"`
		KeepRunning    bool     `json:"keep_running"`
		IdleMS         *int     `json:"idle_ms"`
		ReadyTimeoutMS *int     `json:"ready_timeout_ms"`
		Files          bool     `json:"files"`
		Isolate        bool     `json:"isolate"`
		TimeoutMS      *int     `json:"timeout_ms"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return PublishServiceRequest{}, err
	}
	host, err := requireNonEmpty(args.Host, "host")
	if err != nil {
		return PublishServiceRequest{}, err
	}
	target, err := requireNonEmpty(args.Target, "target")
	if err != nil {
		return PublishServiceRequest{}, err
	}
	if len(args.Env) > maxServiceList || len(args.Listen) > maxServiceList {
		return PublishServiceRequest{}, invalidParam("env and listen take at most %d entries each", maxServiceList)
	}
	timeout, err := parseTimeout(args.TimeoutMS, defaultServiceMS, maxServiceMS)
	if err != nil {
		return PublishServiceRequest{}, err
	}
	return PublishServiceRequest{
		Host: host, Target: target, Route: args.Route, Label: args.Label, Run: args.Run, Cwd: args.Cwd,
		Env: args.Env, Listens: args.Listen, KeepRunning: args.KeepRunning, IdleMS: args.IdleMS,
		ReadyTimeoutMS: args.ReadyTimeoutMS, Files: args.Files, Isolate: args.Isolate, Timeout: timeout,
	}, nil
}

func handleAppsList(ctx context.Context, controls Controls, raw json.RawMessage, _ func(ProgressUpdate)) (CallOutcome, error) {
	host, timeout, err := parseHostTimeoutCall(controls, raw)
	if err != nil {
		return CallOutcome{}, err
	}
	out, err := controls.ListApps(ctx, ListAppsRequest{Host: host, Timeout: timeout})
	return wrapControl("mesh_apps_list", out, err)
}

func parseSessionLifecycleCall(controls Controls, raw json.RawMessage, withAction bool) (SessionLifecycleRequest, error) {
	if err := requireControls(controls); err != nil {
		return SessionLifecycleRequest{}, err
	}
	var args struct {
		Host      string `json:"host"`
		SessionID string `json:"session_id"`
		Action    string `json:"action"`
		TimeoutMS *int   `json:"timeout_ms"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return SessionLifecycleRequest{}, err
	}
	host, err := requireNonEmpty(args.Host, "host")
	if err != nil {
		return SessionLifecycleRequest{}, err
	}
	id, err := requireNonEmpty(args.SessionID, "session_id")
	if err != nil {
		return SessionLifecycleRequest{}, err
	}
	if !withAction && args.Action != "" {
		return SessionLifecycleRequest{}, invalidParam("action applies to mesh_session_recover only")
	}
	if withAction && args.Action != "command" && args.Action != "shell" && args.Action != "agent" {
		return SessionLifecycleRequest{}, invalidParam("action must be command, shell or agent")
	}
	timeout, err := parseTimeout(args.TimeoutMS, 12000, maxTimeoutMS)
	return SessionLifecycleRequest{Host: host, SessionID: id, Action: args.Action, Timeout: timeout}, err
}

func handleSessionHibernate(ctx context.Context, controls Controls, raw json.RawMessage, _ func(ProgressUpdate)) (CallOutcome, error) {
	req, err := parseSessionLifecycleCall(controls, raw, false)
	if err != nil {
		return CallOutcome{}, err
	}
	out, err := controls.HibernateSession(ctx, req)
	return wrapControl("mesh_session_hibernate", out, err)
}

func handleSessionRecover(ctx context.Context, controls Controls, raw json.RawMessage, _ func(ProgressUpdate)) (CallOutcome, error) {
	req, err := parseSessionLifecycleCall(controls, raw, true)
	if err != nil {
		return CallOutcome{}, err
	}
	out, err := controls.RecoverSession(ctx, req)
	return wrapControl("mesh_session_recover", out, err)
}
