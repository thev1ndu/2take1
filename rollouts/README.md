# Argo Rollouts test bed

This folder is a self-contained demo for exercising [Argo Rollouts](https://argo-rollouts.readthedocs.io/)
canary deployments. It swaps a `Deployment` for a `Rollout`, and instead of building
new container images for each version, it serves two versions of a static
`index.html` from ConfigMaps — so a "release" is just flipping which ConfigMap
is mounted.

## Layout

```
rollouts/
  kustomization.yaml          # entrypoint: kubectl apply -k rollouts/
  manifests/
    namespace.yaml            # 2take1 namespace
    configmap-v1.yaml         # index.html, green "(v1 / stable)"
    configmap-v2.yaml         # index.html, orange "(v2 / canary)"
    service.yaml              # single Service, routes to whatever pods exist
    rollout.yaml               # the Rollout itself (canary strategy)
```

## 0. Prerequisites (one-time, cluster-wide)

`kind: Rollout` is a CRD — nothing in this folder installs the controller,
because that's a cluster concern, not an app concern. Install it once per
cluster:

```bash
kubectl create namespace argo-rollouts
kubectl apply -n argo-rollouts -f https://github.com/argoproj/argo-rollouts/releases/latest/download/install.yaml
```

And install the `kubectl argo rollouts` plugin, which gives you a live
terminal dashboard for watching canaries progress (not required, but you
want it):

```bash
brew install argoproj/tap/kubectl-argo-rollouts
```

## 1. How it works

- A `Rollout` (manifests/rollout.yaml) is a drop-in replacement for a
  `Deployment`. Same `replicas`/`selector`/`template` fields, plus a
  `strategy` block the Rollout controller uses to manage the transition
  between old and new pods instead of the default rolling update.
- The pod template mounts one file, `index.html`, from a ConfigMap volume
  (`configMap.name: web-html-v1`). There's no image to build or tag — the
  "new version" is just a different ConfigMap's content.
- `strategy.canary.steps` defines a script the controller runs whenever the
  pod template changes:
  ```yaml
  steps:
    - setWeight: 20
    - pause: { duration: 30s }
    - setWeight: 50
    - pause: { duration: 30s }
    - setWeight: 80
    - pause: { duration: 30s }
  ```
  At `setWeight: 20`, the controller scales up a *new* ReplicaSet ("canary")
  to ~20% of `replicas` and scales the *old* one ("stable") down to ~80%,
  then waits at the `pause` until you (or an automated `pause.duration`)
  let it continue. It repeats this at each step until it reaches 100%, at
  which point the old ReplicaSet is scaled to zero.
- There's a single `Service` here with no `trafficRouting` configured in
  the Rollout spec. That means this is **basic canary**: both old and new
  pods share the same `app: 2take1` label and the one Service load
  balances across whichever pods currently exist. The "weight" is
  approximate and controlled purely by relative pod counts — good enough
  for testing the mechanics without wiring up Ingress/mesh traffic
  splitting (Traefik/NGINX/Istio, etc.), which this demo intentionally
  skips.

## 2. Deploy it

```bash
kubectl apply -k rollouts/
```

This creates the namespace, both ConfigMaps, the Service, and the Rollout.
Since it's a brand-new Rollout (no prior revision), the controller skips
the canary steps and goes straight to `replicas` pods at v1 — canary steps
only kick in on *changes* to an existing Rollout.

Check it came up:

```bash
kubectl argo rollouts get rollout 2take1 -n 2take1
```

## 3. Trigger a rollout (v1 → v2)

Edit `manifests/rollout.yaml` and change the volume's ConfigMap reference:

```diff
       volumes:
         - name: html
           configMap:
-            name: web-html-v1
+            name: web-html-v2
```

Apply it:

```bash
kubectl apply -k rollouts/
```

Changing that field changes the pod template hash, so the controller treats
it exactly like a new image tag: it creates a new ("canary") ReplicaSet
running v2 and starts walking through the `steps`.

Watch it live:

```bash
kubectl argo rollouts get rollout 2take1 -n 2take1 --watch
```

You'll see something like:

```
NAME                              KIND        STATUS     WEIGHT
⟳ 2take1                          Rollout     ॥ Paused  20
├──# revision:2
│  └──⧉ 2take1-6d8f...             ReplicaSet  ✔ Healthy  20
└──# revision:1
   └──⧉ 2take1-7c9b... (stable)    ReplicaSet  ✔ Healthy  80
```

To see the actual page flip, port-forward the Service and refresh a few
times while it's paused mid-rollout — you should get a mix of the green
"(v1 / stable)" page and the orange "(v2 / canary)" page:

```bash
kubectl port-forward -n 2take1 svc/2take1 8080:80
curl -s localhost:8080 | grep -E 'v1|v2'
```

## 4. Controlling the rollout

- **Let it run on its own**: since every step has a `pause: { duration: 30s }`,
  it'll auto-advance through 20% → 50% → 80% → 100% without any input,
  ~90s total.
- **Promote a paused step immediately** (skip the wait):
  ```bash
  kubectl argo rollouts promote 2take1 -n 2take1
  ```
- **Promote straight to 100%**, skipping remaining steps:
  ```bash
  kubectl argo rollouts promote 2take1 -n 2take1 --full
  ```
- **Abort** (if v2 looks broken, roll back to stable immediately):
  ```bash
  kubectl argo rollouts abort 2take1 -n 2take1
  ```
- **Undo** (revert to the previous revision after a completed rollout):
  ```bash
  kubectl argo rollouts undo 2take1 -n 2take1
  ```

## 5. Clean up

```bash
kubectl delete -k rollouts/
kubectl delete namespace 2take1
```

(The namespace delete is belt-and-suspenders in case `kubectl apply -k`
ever partially failed; `kubectl delete -k` already removes it via
manifests/namespace.yaml.)
