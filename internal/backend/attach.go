package backend

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
)

// AttachCommand returns an exact SSH argv. Only non-secret ownership metadata is
// sent as arguments; the operator terminal remains stdin for the remote tmux.
func (p Proxmox) AttachCommand(id string, spec ContainerSpec) ([]string, error) {
	if !hostPattern.MatchString(p.Host) {
		return nil, errors.New("invalid SSH host")
	}
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	if spec.Tags == nil {
		spec.Tags = []string{}
	}
	payload, err := json.Marshal(map[string]any{"operation": "guest", "deployment": id, "spec": spec, "command": []string{"runuser", "-u", "agent", "--", "tmux", "attach-session", "-t", "hrx-" + id}})
	if err != nil {
		return nil, err
	}
	// Run the same ownership checker as other operations, under its lifecycle lock.
	// Keep the lock until detach so the VMID cannot be destroyed/reused mid-attach.
	script := `import json,os,subprocess,sys,types
module=types.ModuleType('hrx_host')
exec(sys.argv[1],module.__dict__)
os.umask(0o077)
def attach(argv):
    status=subprocess.call(argv)
    if status: raise module.Refused('native attach exited unsuccessfully')
    return ''
try:
    module.Host('/var/lib/hrx/proxmox',guest=attach).handle(json.loads(sys.argv[2]))
except module.Refused as error:
    print(str(error),file=sys.stderr);sys.exit(1)
except Exception:
    print('remote attach failed',file=sys.stderr);sys.exit(1)
`
	command, err := remoteCommand([]string{"python3", "-c", script, proxmoxHelper, string(payload)})
	if err != nil {
		return nil, err
	}
	return []string{"-tt", "-o", "BatchMode=yes", "-o", "ConnectTimeout=10", "--", p.Host, command}, nil
}

// Attach connects to the existing terminal. Detaching does not stop the worker.
func (p Proxmox) Attach(ctx context.Context, id string, spec ContainerSpec, input *os.File, out, errOut io.Writer) error {
	args, err := p.AttachCommand(id, spec)
	if err != nil {
		return err
	}
	command := exec.CommandContext(ctx, "ssh", args...)
	command.Stdin = input
	command.Stdout = out
	command.Stderr = errOut
	if err = command.Run(); err != nil {
		return errors.New("remote attach ended unsuccessfully; worker lifecycle is unchanged")
	}
	return nil
}
