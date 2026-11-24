# Install

This page says how to run the coordinator in a Kubernetes cluster.

No image is published yet. Until the first release you build the image
yourself and give it to your cluster.

## What is installed

| Directory | What it holds |
|---|---|
| `config/crd` | The resource types of `allostat.io`. |
| `config/rbac` | The role `allostat-coordinator`: what the coordinator can read and write. |
| `config/manager` | The namespace `allostat-system`, the account of the coordinator, the binding of the role to it, and the deployment. |

The coordinator reads and decides in every namespace. It needs no access
to your targets and has none.

## Steps

1. Build the image. This needs Docker.

   ```sh
   make image
   ```

   The image is `ghcr.io/jpaiv/allostat:dev`. It exists only on your
   machine.

2. Give the image to the cluster. On a local `kind` cluster:

   ```sh
   kind load docker-image ghcr.io/jpaiv/allostat:dev
   ```

   For another cluster, push the image to a registry the cluster can
   read, and put that name in `config/manager/deployment.yaml`.

3. Install. The resource types go first.

   ```sh
   kubectl apply -f config/crd
   kubectl apply -f config/rbac -f config/manager
   ```

4. Check that it runs.

   ```sh
   kubectl -n allostat-system get pods
   ```

   Both pods must be `Running` and ready. One of them decides; the other
   waits to take over.

## Two processes

The deployment runs two coordinators with `--leader-elect=true`. Only the
one that holds the lock decides. When it is gone, the other takes the lock
and goes on from the requests and the ledger in the cluster. In between,
requests wait and no new action starts. See
[When there is no answer](no-answer.md).

Do not turn leader election off with more than one process. The ledger
guard still keeps the budgets, but two coordinators then do the same work
twice.

## Flags

| Flag | Default | What it does |
|---|---|---|
| `--leader-elect` | `true` | Only one coordinator decides. |
| `--release-retry` | `30s` | Wait before the coordinator looks again for evidence that closes an overdue or failed grant. |
| `--metrics-bind-address` | `:8080` | Address of the metrics endpoint. |
| `--health-probe-bind-address` | `:8081` | Address of `/healthz` and `/readyz`. |

## Access for everyone else

The install gives rights only to the coordinator. You decide who writes
the rest:

- **Contracts:** the people who own the rules of the fleet.
- **Observations:** each reporter, for its own conditions.
- **Action requests:** each controller that wants to act. It needs
  `create`, `get` and `update` on `actionrequests`, and nothing on
  `actionrequests/status`.
- **Ledgers and records:** nobody but the coordinator.

## Remove

```sh
kubectl delete -f config/manager -f config/rbac
kubectl delete -f config/crd
```

The second command deletes the resource types, and with them every
contract, request, ledger and record in the cluster.
