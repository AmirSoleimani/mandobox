# Getting started with Mandobox

This is the friendly, start-to-finish path. It's still infrastructure — you're setting up a real
server — but you don't need to understand every internal to get it running. Each step links to a
detailed guide when you want more.

## What you'll need

- **A dedicated server that supports hardware virtualization** (a real `/dev/kvm`). A Hetzner
  dedicated box works; most cloud VMs do **not** (no nested virtualization). See
  [server setup](hetzner-setup.md).
- **A GitHub App** on the org whose repos you want the agents to work on. See
  [GitHub setup](github-setup.md).
- **An Anthropic API key** (with credit on it).
- **Optionally, a Slack workspace** if you want to drive tasks from Slack. See [Slack](slack.md).
- On your own laptop: Go 1.25+ and Ansible, to build and deploy.

## The four steps

### 1. Prepare the server
Provision the box (KVM, Firecracker, networking). Follow [server setup](hetzner-setup.md), then the
provisioning section of the [operator runbook](runbook.md).

### 2. Give it credentials
Create your GitHub App and drop your keys into `secrets/` (never committed — it's gitignored):

```sh
printf '%s' "sk-ant-..."  > secrets/anthropic.key            # your Anthropic key
# your GitHub App private key .pem also goes in secrets/
```

Put your host and GitHub App details in `ansible/inventory/local.yml` (gitignored) — the runbook
shows the exact fields.

### 3. Deploy
From your laptop, build the binaries and run the playbooks against your server:

```sh
make dist
cd ansible
ansible-playbook -i inventory/local.yml site.yml          # provision
ansible-playbook -i inventory/local.yml build-image.yml   # build the guest image
ansible-playbook -i inventory/local.yml deploy.yml        # bring the services up
```

### 4. Run your first task
Point it at a repo and describe a change — two ways in:

- **Dashboard** — open an SSH tunnel to the box (`ssh -L 8087:127.0.0.1:8087 root@<host>`), visit
  `http://localhost:8087`, and hit **+ New session**: pick the repo, write the prompt, and dispatch.
- **Slack** — if you set it up, message the bot in a channel: `/mando <owner>/<repo> <what to do>`.

Either way, in a minute or two you'll get a **pull request** to review. Comment on it and the agent
replies in the thread; merge when you're happy, and it cleans itself up.

## Everyday use

- **Start a task:** the dashboard's **+ New session**, or Slack.
- **Steer it:** reply in the Slack thread or comment on the pull request — the agent reads both.
- **Keep a machine alive to poke at it:** set **Keep-alive** to `never` when you start the task (see the runbook).
- **Control what the internet the agents can reach:** the egress policy (`strict` vs `open`) — see
  the "Egress policy" section of the [runbook](runbook.md).

## When something goes wrong

- Watch a task live in the Temporal UI (`ssh -L 8233:127.0.0.1:8233 root@<host>` → `localhost:8233`).
- Check service logs on the server: `journalctl -u mando-worker -f`, `journalctl -u mando-gateway -f`.
- The [operator runbook](runbook.md) has the deeper troubleshooting and every configuration knob.
