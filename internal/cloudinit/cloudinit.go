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
	"regexp"
	"strings"
	"text/template"
	"time"
)

// Options for rendering.
type Options struct {
	User             string        // login user that receives the SSH key
	PublicKey        string        // authorized_keys line
	IdleTimeout      time.Duration // shut down after this much inactivity
	MaxLifetime      time.Duration // hard cap regardless of activity
	SelfDestruct     string        // shell command; default "systemctl poweroff"
	BootstrapScript  string        // runs as root after the watchdog is armed
	BootstrapTimeout time.Duration // per bootstrap script (default 15m)
	UserBootstrap    string        // runs as User (login shell) after BootstrapScript
}

// Background apt jobs (unattended-upgrades, apt-daily) are stopped at boot:
// they would hold the dpkg lock against the bootstrap and the user's
// commands. VMs are short-lived (max_lifetime), so OS updates come from the
// image instead: an unbaked VM boots Canonical's current Ubuntu image, and a
// bake applies security updates when it is made and stops being used once a
// newer base image has been out a week, or after 30 days (see
// provider.BakeMaxAge).
var tmpl = template.Must(template.New("ud").Parse(`#!/bin/bash
# spotexec ephemeral runner bootstrap (generated). Safe to read: it arms an
# idle watchdog first, then authorizes the spotexec SSH key, then runs the
# user's bootstrap scripts.
set -u
D=/var/tmp/spotexec
# /var/tmp is world-writable, so on a custom image another account may
# already have put a directory, file or symlink at $D, or files in a
# world-writable one (a planted created_at -> /etc/passwd, written below as
# root). The helper trusts only a real directory root owns that others
# can't write to, and so does everything below: anything else is removed
# (a link itself, never its target). Once root owns the entry, /var/tmp's
# sticky bit keeps everyone else from replacing it.
sx_rootdir() { [ ! -L "$D" ] && [ -d "$D" ] && [ "$(stat -c %u "$D")" = 0 ] && [ -z "$(find "$D" -maxdepth 0 -perm -0002)" ]; }
for _ in 1 2 3; do
  if [ -e "$D" ] || [ -L "$D" ]; then sx_rootdir || timeout 60 rm -rf "$D"; fi
  mkdir -p "$D" 2>/dev/null
  sx_rootdir && break
done
if ! sx_rootdir; then
  # The watchdog isn't armed yet: nothing else would ever stop this VM.
  logger -t spotexec "$D is not a directory root owns; shutting down" 2>/dev/null
  {{.SelfDestruct}}
  sleep 120; poweroff -f
  exit 1
fi
chmod 0755 "$D"   # opened to the runner user's group below
# User-data runs once per instance, so this is the instance's own birth time
# (an AMI baked from a runner must not inherit an old one).
date +%s > "$D/created_at"
touch "$D/lifecycle.lock" "$D/activity"
chmod 0644 "$D/lifecycle.lock"
chmod 0644 "$D/activity"
echo "{{.IdleText}}" > "$D/idle_timeout"
echo "{{.MaxSecs}}" > "$D/max_lifetime"   # the helper stops taking new runs shortly before it
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
  local last n
  n=$(date +%s)
  last=$(stat -c %Y "$D/activity" 2>/dev/null || echo "$created")
  # A clock stepped backwards must not leave activity "in the future"
  # (which would postpone idle shutdown by the size of the jump).
  if [ "$last" -gt "$n" ]; then last=$n; touch "$D/activity" 2>/dev/null; fi
  echo $(( n - last ))
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
# No default dependencies, and SIGTERM ignored: shutdown must not stop the
# timer before it fires, nor kill the forced poweroff that backs it up.
systemd-run --unit=spotexec-maxlife --on-active={{.MaxSecs}} -p DefaultDependencies=no --timer-property=DefaultDependencies=no /bin/sh -c 'trap "" TERM HUP; touch /var/tmp/spotexec/draining; {{.SelfDestruct}}; sleep 120; poweroff -f' >/dev/null 2>&1 || true

# ---- 2. SSH access for the spotexec-managed key ----
U={{.User}}
# Public-key logins as the runner user only, whatever the image's sshd
# defaults. sshd keeps the first value it reads, so these go in a drop-in
# that sorts first (cloud-init's 50-cloud-init.conf may allow passwords), or
# at the top of sshd_config if a setting before its Include (or no Include)
# still wins. An sshd that rejects them (an explicit "AuthenticationMethods
# any") is retried without AuthenticationMethods, then left as the image had
# it: the key is never locked out. What may still allow more (Match blocks,
# other AllowUsers) is logged. A root runner user may log in with its key.
# Many agents may connect at once; the default MaxStartups (10:30:100) would
# drop some handshakes. Every run from one machine shares its ControlMaster
# connection, so MaxSessions (default 10 channels per connection) must allow
# all slots plus status probes and helper uploads. ClientAlive: a vanished
# client (laptop asleep) is dropped in ~90s, so its session stops holding
# the lifecycle lock and the idle clock can run.
C=/etc/ssh/sshd_config F=/etc/ssh/sshd_config.d/00-spotexec.conf S=/usr/sbin/sshd L=/var/log/spotexec-bootstrap.log
R=no RT=no; [ "$U" = root ] && R=prohibit-password RT=without-password
printf '%s\n' 'AuthenticationMethods publickey' 'PubkeyAuthentication yes' 'PasswordAuthentication no' 'KbdInteractiveAuthentication no' 'ChallengeResponseAuthentication no' 'HostbasedAuthentication no' 'GSSAPIAuthentication no' 'PermitEmptyPasswords no' "PermitRootLogin $R" "AllowUsers $U" 'MaxStartups 100:30:200' 'MaxSessions 256' 'ClientAliveInterval 30' 'ClientAliveCountMax 3' > "$D/sshd.conf"
sshd_ok() { "$S" -T -C "user=$U,host=localhost,addr=127.0.0.1" 2>/dev/null | grep -E '^(allowusers|authenticationmethods|passwordauthentication|permitrootlogin|pubkeyauthentication) ' | LC_ALL=C sort | tr '\n' ';' | grep -qx "allowusers $U;authenticationmethods publickey;passwordauthentication no;permitrootlogin $RT;pubkeyauthentication yes;"; }
unharden() { rm -f "$F"; sed -i '/^# spotexec begin$/,/^# spotexec end$/d' "$C"; }
harden() {
  unharden
  grep -qiE '^[[:space:]]*Include[[:space:]]+/etc/ssh/sshd_config\.d/\*' "$C" && [ -d "${F%/*}" ] && cp "$1" "$F"
  if ! sshd_ok; then
    rm -f "$F"
    { echo '# spotexec begin'; cat "$1"; echo '# spotexec end'; cat "$C"; } > "$C.sx" && cat "$C.sx" > "$C"; rm -f "$C.sx"
  fi
  "$S" -t 2>>"$L" || { unharden; return 1; }
}
install -d -m 0755 /run/sshd
rm -f /etc/ssh/sshd_config.d/60-spotexec.conf
grep -v '^AuthenticationMethods' "$D/sshd.conf" > "$D/sshd-compat.conf"
harden "$D/sshd.conf" || harden "$D/sshd-compat.conf" || echo "spotexec: sshd rejected the hardened settings; left as the image had them" >> "$L"
sshd_ok || echo "spotexec: warning: sshd allows more than public-key logins as $U (see sshd -T)" >> "$L"
grep -qisE '^[[:space:]]*Match[[:space:]]' "$C" /etc/ssh/sshd_config.d/*.conf 2>/dev/null && echo "spotexec: warning: the image's sshd Match blocks can override key-only logins" >> "$L"
systemctl reload ssh >/dev/null 2>&1 || systemctl reload sshd >/dev/null 2>&1 || true
# The runner user (the helper) owns its state files; others can't drain or
# fake them. The bootstrap log stays readable to it, not to everyone.
chgrp "$(id -gn "$U")" "$D" "$D/activity" && chmod 0775 "$D" && chmod 0664 "$D/activity"
touch /var/log/spotexec-bootstrap.log
chgrp "$(id -gn "$U")" /var/log/spotexec-bootstrap.log && chmod 0640 /var/log/spotexec-bootstrap.log
H=$(getent passwd "$U" | cut -d: -f6)
if [ -n "$H" ]; then
  install -d -m 0700 -o "$U" -g "$(id -gn "$U")" "$H/.ssh"
  grep -qxF '{{.PublicKey}}' "$H/.ssh/authorized_keys" 2>/dev/null || echo '{{.PublicKey}}' >> "$H/.ssh/authorized_keys"
  chown "$U:$(id -gn "$U")" "$H/.ssh/authorized_keys"
  chmod 0600 "$H/.ssh/authorized_keys"
fi

# ---- 3. localhost on IPv6 too ----
# Ubuntu maps localhost to 127.0.0.1 only; macOS and CI runners also map it
# to ::1, so a test server bound to ::1 and dialed as localhost would fail
# only here. Project containers (host networking) copy this file. Only with
# an IPv6 loopback; written in place (a bind mount in the simulator). This
# runs on baked boots too; cloud-init rewrites the file only with
# manage_etc_hosts, from a template that already maps ::1 to localhost.
if grep -q '^00000000000000000000000000000001 ' /proc/net/if_inet6 2>/dev/null && ! grep -qE '^[[:space:]]*::1[[:space:]]([^#]*[[:space:]])?localhost([[:space:]]|$)' /etc/hosts; then
  if grep -qE '^[[:space:]]*::1([[:space:]]|$)' /etc/hosts; then
    awk '!d && $1 == "::1" { sub(/::1/, "::1 localhost"); d = 1 } { print }' /etc/hosts > /etc/hosts.sx && cat /etc/hosts.sx > /etc/hosts
    rm -f /etc/hosts.sx
  else
    { [ -z "$(tail -c1 /etc/hosts)" ] || echo; echo '::1 localhost'; } >> /etc/hosts
  fi
fi

# ---- 4. User bootstrap ----
# Bootstrap runs under a shared lifecycle lock so the idle watchdog cannot
# power the VM off mid-setup when setup takes longer than idle_timeout
# (max_lifetime still applies); taken before stopping apt, which can block
# for minutes on first boot. The scripts themselves get fd 8 closed so a
# daemon they start cannot hold the lock forever.
touch "$D/activity"
exec 8<"$D/lifecycle.lock"
flock -s 8
# No background apt (see the note in cloudinit.go): updates come with the image.
# Step markers in the log: a waiting client shows its latest line.
B=/var/log/spotexec-bootstrap.log
echo "spotexec: waiting for the OS's automatic updates to stop" >>"$B"
systemctl stop unattended-upgrades.service apt-daily.service apt-daily-upgrade.service >/dev/null 2>&1 || true
systemctl disable --now apt-daily.timer apt-daily-upgrade.timer >/dev/null 2>&1 || true
status=0
{{- if .BootstrapScript}}
umask 077
cat > "$D/bootstrap-root.sh" <<'BOOTSTRAP_ROOT'
{{.BootstrapScript}}
BOOTSTRAP_ROOT
umask 022
echo "spotexec: running bootstrap_script" >>"$B"
DEBIAN_FRONTEND=noninteractive timeout -k 30 {{.BootSecs}} bash -e "$D/bootstrap-root.sh" </dev/null >>/var/log/spotexec-bootstrap.log 2>&1 8<&- || status=$?
{{- end}}
{{- if .UserBootstrap}}
umask 077
cat > "$D/bootstrap-user.sh" <<'BOOTSTRAP_USER'
{{.UserBootstrap}}
BOOTSTRAP_USER
umask 022
chown "$U" "$D/bootstrap-user.sh"
if [ "$status" -eq 0 ]; then
  echo "spotexec: running user_bootstrap_script" >>"$B"
  timeout -k 30 {{.BootSecs}} sudo -u "$U" -H bash -le "$D/bootstrap-user.sh" </dev/null >>/var/log/spotexec-bootstrap.log 2>&1 8<&- || status=$?
fi
{{- end}}
echo "spotexec: first-boot setup finished (exit $status)" >>"$B"
chmod 0640 /var/log/spotexec-bootstrap.log 2>/dev/null || true
touch "$D/activity"
exec 8<&-
echo "$status" > "$D/ready"
`))

var userRe = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)

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
	if !userRe.MatchString(o.User) {
		return "", fmt.Errorf("invalid user %q", o.User)
	}
	for _, s := range []string{o.BootstrapScript, o.UserBootstrap} {
		// Any line equal to a terminator (after trimming, as the shell
		// compares) would end the heredoc early and run the rest as root.
		for _, line := range strings.Split(s, "\n") {
			if t := strings.TrimSpace(line); t == "BOOTSTRAP_ROOT" || t == "BOOTSTRAP_USER" {
				return "", fmt.Errorf("bootstrap script may not contain a line %q", t)
			}
		}
	}
	if o.BootstrapTimeout <= 0 {
		o.BootstrapTimeout = 15 * time.Minute
	}
	data := map[string]any{
		"BootSecs": int(o.BootstrapTimeout.Seconds()),
		"User":     o.User, "PublicKey": o.PublicKey, "SelfDestruct": o.SelfDestruct,
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
