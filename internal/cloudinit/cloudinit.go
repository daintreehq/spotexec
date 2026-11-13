// Package cloudinit renders the first-boot user-data script for ephemeral
// runners.
//
// The script's first action is installing an idle watchdog that needs nothing
// from spotexec (no helper binary, no laptop): a systemd timer that powers the
// VM off when it has been idle for idle_timeout, and unconditionally after
// max_lifetime. On AWS, instances are launched with
// InstanceInitiatedShutdownBehavior=terminate, so power-off == terminate and
// billing stops without any credentials on the VM.
//
// Idle detection: spotexec-helper touches /var/tmp/spotexec/activity on every
// session and every 20s while a session is open, and holds a shared flock on
// /var/tmp/spotexec/lifecycle.lock while it owns a slot. The watchdog only
// shuts down if it can take that lock exclusively (no session in progress) and
// activity is stale; it writes /var/tmp/spotexec/draining first so helpers
// refuse new work. The max_lifetime cap ignores the lock.
package cloudinit

import (
	"bytes"
	"fmt"
	"strings"
	"text/template"
	"time"
)

// Options for rendering.
type Options struct {
	User            string        // login user that receives the SSH key
	PublicKey       string        // authorized_keys line
	IdleTimeout     time.Duration // shut down after this much inactivity
	MaxLifetime     time.Duration // hard cap regardless of activity
	SelfDestruct    string        // shell command; default "systemctl poweroff"
	BootstrapScript string        // runs as root after the watchdog is armed
	UserBootstrap   string        // runs as User (login shell) after BootstrapScript
}

var tmpl = template.Must(template.New("ud").Parse(`#!/bin/bash
# spotexec ephemeral runner bootstrap (generated). Safe to read: it arms an
# idle watchdog first, then authorizes the spotexec SSH key, then runs the
# user's bootstrap scripts.
set -u
D=/var/tmp/spotexec
mkdir -p "$D"
chmod 1777 "$D"
# User-data runs once per instance, so this is the instance's own birth time
# (an AMI baked from a runner must not inherit an old one).
date +%s > "$D/created_at"
touch "$D/lifecycle.lock" "$D/activity"
chmod 0644 "$D/lifecycle.lock"
chmod 0666 "$D/activity"
echo "{{.IdleText}}" > "$D/idle_timeout"
rm -f "$D/draining" "$D/ready"

# ---- 1. Idle watchdog (armed before anything else can fail) ----
cat > /usr/local/sbin/spotexec-watchdog <<'WATCHDOG'
#!/bin/bash
D=/var/tmp/spotexec
IDLE={{.IdleSecs}}
MAXLIFE={{.MaxSecs}}
now=$(date +%s)
created=$(cat "$D/created_at" 2>/dev/null || echo "$now")
destroy() {
  touch "$D/draining"
  logger -t spotexec-watchdog "$1; shutting down"
  {{.SelfDestruct}}
  exit 0
}
if [ $((now - created)) -ge "$MAXLIFE" ]; then
  destroy "max lifetime ({{.MaxText}}) reached"
fi
# Draining (spot rebalance/interruption, or shutdown already decided): leave
# as soon as no session holds the lifecycle lock, without waiting for idle.
if [ -f "$D/draining" ]; then
  exec 9<"$D/lifecycle.lock"
  flock -n -x 9 && destroy "draining with no active sessions"
  exit 0
fi
idle_for() {
  local last
  last=$(stat -c %Y "$D/activity" 2>/dev/null || echo "$created")
  echo $(( $(date +%s) - last ))
}
[ "$(idle_for)" -ge "$IDLE" ] || exit 0
# An active spotexec session holds this lock shared; only shut down if none do.
exec 9<"$D/lifecycle.lock"
flock -n -x 9 || exit 0
[ "$(idle_for)" -ge "$IDLE" ] || exit 0
destroy "idle for $(idle_for)s (limit {{.IdleText}})"
WATCHDOG
chmod 0755 /usr/local/sbin/spotexec-watchdog

cat > /etc/systemd/system/spotexec-watchdog.service <<'UNIT'
[Unit]
Description=spotexec idle watchdog
[Service]
Type=oneshot
ExecStart=/usr/local/sbin/spotexec-watchdog
UNIT
cat > /etc/systemd/system/spotexec-watchdog.timer <<'UNIT'
[Unit]
Description=spotexec idle watchdog timer
[Timer]
OnBootSec=60
OnUnitActiveSec=60
AccuracySec=5
[Install]
WantedBy=timers.target
UNIT
systemctl daemon-reload
systemctl enable --now spotexec-watchdog.timer
# Independent backstop in case the timer unit is ever broken.
systemd-run --unit=spotexec-maxlife --on-active={{.MaxSecs}} /bin/sh -c 'touch /var/tmp/spotexec/draining; {{.SelfDestruct}}' >/dev/null 2>&1 || true

# ---- 2. SSH access for the spotexec-managed key ----
U={{.User}}
H=$(getent passwd "$U" | cut -d: -f6)
if [ -n "$H" ]; then
  install -d -m 0700 -o "$U" -g "$(id -gn "$U")" "$H/.ssh"
  grep -qxF '{{.PublicKey}}' "$H/.ssh/authorized_keys" 2>/dev/null || echo '{{.PublicKey}}' >> "$H/.ssh/authorized_keys"
  chown "$U:$(id -gn "$U")" "$H/.ssh/authorized_keys"
  chmod 0600 "$H/.ssh/authorized_keys"
fi

# ---- 3. User bootstrap ----
systemctl stop unattended-upgrades.service apt-daily.service apt-daily-upgrade.service >/dev/null 2>&1 || true
systemctl disable --now apt-daily.timer apt-daily-upgrade.timer >/dev/null 2>&1 || true
status=0
# Bootstrap runs under a shared lifecycle lock so the idle watchdog cannot
# power the VM off mid-setup when setup takes longer than idle_timeout
# (max_lifetime still applies). The scripts themselves get fd 8 closed so a
# daemon they start cannot hold the lock forever.
touch "$D/activity"
exec 8<"$D/lifecycle.lock"
flock -s 8
{{- if .BootstrapScript}}
umask 077
cat > "$D/bootstrap-root.sh" <<'BOOTSTRAP_ROOT'
{{.BootstrapScript}}
BOOTSTRAP_ROOT
umask 022
bash -e "$D/bootstrap-root.sh" >>/var/log/spotexec-bootstrap.log 2>&1 8<&- || status=$?
{{- end}}
{{- if .UserBootstrap}}
umask 077
cat > "$D/bootstrap-user.sh" <<'BOOTSTRAP_USER'
{{.UserBootstrap}}
BOOTSTRAP_USER
umask 022
chown "$U" "$D/bootstrap-user.sh"
if [ "$status" -eq 0 ]; then
  sudo -u "$U" -H bash -le "$D/bootstrap-user.sh" >>/var/log/spotexec-bootstrap.log 2>&1 8<&- || status=$?
fi
{{- end}}
chmod 0644 /var/log/spotexec-bootstrap.log 2>/dev/null || true
touch "$D/activity"
exec 8<&-
echo "$status" > "$D/ready"
`))

// Render produces the user-data script.
func Render(o Options) (string, error) {
	if o.SelfDestruct == "" {
		o.SelfDestruct = "systemctl poweroff"
	}
	if o.IdleTimeout <= 0 {
		o.IdleTimeout = 20 * time.Minute
	}
	if o.MaxLifetime <= 0 {
		o.MaxLifetime = 12 * time.Hour
	}
	if strings.ContainsAny(o.PublicKey, "'\n") || o.PublicKey == "" {
		return "", fmt.Errorf("invalid public key")
	}
	if strings.ContainsAny(o.User, "'\"$ \n;") || o.User == "" {
		return "", fmt.Errorf("invalid user %q", o.User)
	}
	for _, s := range []string{o.BootstrapScript, o.UserBootstrap} {
		if strings.Contains(s, "\nBOOTSTRAP_ROOT") || strings.Contains(s, "\nBOOTSTRAP_USER") {
			return "", fmt.Errorf("bootstrap script may not contain heredoc terminators")
		}
	}
	data := map[string]any{
		"User": o.User, "PublicKey": o.PublicKey, "SelfDestruct": o.SelfDestruct,
		"IdleSecs": int(o.IdleTimeout.Seconds()), "MaxSecs": int(o.MaxLifetime.Seconds()),
		"IdleText": o.IdleTimeout.String(), "MaxText": o.MaxLifetime.String(),
		"BootstrapScript": strings.TrimSpace(o.BootstrapScript), "UserBootstrap": strings.TrimSpace(o.UserBootstrap),
	}
	var b bytes.Buffer
	if err := tmpl.Execute(&b, data); err != nil {
		return "", err
	}
	if b.Len() > 16*1024 {
		return "", fmt.Errorf("user-data is %d bytes; AWS allows 16 KiB (shorten bootstrap_script)", b.Len())
	}
	return b.String(), nil
}
