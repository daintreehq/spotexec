# Test runner image for scripts/e2e.sh: a plain Ubuntu 24.04 SSH box, like a
# homelab machine or a fresh cloud VM.
FROM ubuntu:24.04
RUN apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
      openssh-server ca-certificates nodejs npm python3 && \
    rm -rf /var/lib/apt/lists/* && mkdir -p /run/sshd && \
    useradd -m -s /bin/bash dev && mkdir -p /home/dev/.ssh && chmod 700 /home/dev/.ssh
COPY id_ed25519.pub /home/dev/.ssh/authorized_keys
RUN chown -R dev:dev /home/dev/.ssh && chmod 600 /home/dev/.ssh/authorized_keys
EXPOSE 22
CMD ["/usr/sbin/sshd", "-D", "-e"]
