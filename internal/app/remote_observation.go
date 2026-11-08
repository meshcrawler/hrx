package app

import (
	"encoding/json"
	"errors"
	"io"

	"github.com/meshcrawler/hrx/internal/monitor"
)

// remoteObservation lets an external controller (such as an experiment driver)
// record the same dashboard rows that hrx run writes for its own deployments.
type remoteObservation struct {
	Operation  string `json:"operation"`
	Deployment string `json:"deployment"`
	Host       string `json:"host"`
	Name       string `json:"name"`
	Identity   string `json:"identity"`
	Harness    string `json:"harness"`
	Model      string `json:"model"`
	Stage      string `json:"stage"`
	Failed     bool   `json:"failed"`
	VMID       int    `json:"vmid"`
	Session    string `json:"session"`
}

func runRemoteObservation(in io.Reader) error {
	decoder := json.NewDecoder(io.LimitReader(in, 64*1024))
	decoder.DisallowUnknownFields()
	var o remoteObservation
	if err := decoder.Decode(&o); err != nil {
		return errors.New("invalid remote observation")
	}
	store, err := monitor.Default()
	if err != nil {
		return err
	}
	switch o.Operation {
	case "register":
		switch o.Harness {
		case "codex", "claude", "opencode":
		default:
			return errors.New("invalid remote observation harness")
		}
		return store.RegisterRemote(o.Deployment, o.Host, o.Name, o.Identity, o.Harness, o.Model)
	case "stage":
		if o.VMID < 0 || len(o.Session) > 256 {
			return errors.New("invalid remote observation")
		}
		return store.UpdateRemote(o.Deployment, o.Stage, o.Failed, o.VMID, o.Session)
	}
	return errors.New("remote observation operation must be register or stage")
}
