# Test runner image for scripts/e2e.sh: a plain Ubuntu 24.04 SSH box, like a
# homelab machine or a fresh cloud VM.
FROM ubuntu:24.04
# fishy and cshy log in with non-POSIX shells, as some homelab users do.
RUN apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
      openssh-server ca-certificates nodejs npm python3 fish tcsh && \
    rm -rf /var/lib/apt/lists/* && mkdir -p /run/sshd && \
    useradd -m -s /bin/bash dev && useradd -m -s /usr/bin/fish fishy && useradd -m -s /usr/bin/tcsh cshy
COPY id_ed25519.pub /tmp/key.pub
RUN for u in dev fishy cshy; do install -d -m 700 -o $u -g $u /home/$u/.ssh && \
      install -m 600 -o $u -g $u /tmp/key.pub /home/$u/.ssh/authorized_keys; done
EXPOSE 22
CMD ["/usr/sbin/sshd", "-D", "-e"]
