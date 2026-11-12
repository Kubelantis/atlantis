# Deployment

This page covers getting Atlantis up and running in your infrastructure.

::: tip Prerequisites

* You have created [access credentials](access-credentials.md) for your Atlantis user
* You have created a [webhook secret](webhook-secrets.md)
:::

## Architecture Overview

### Runtime

Atlantis is a [Go](https://golang.org/) app. It receives webhooks from your Git
host and executes Terraform commands locally. This fork runs only in Kubernetes:
it keeps locks, pull request status and cluster membership in the Kubernetes
API, so several replicas can run at once. Its image is
`ghcr.io/kubelantis/katlantis` (see [Docker image](#docker-image)).

### Routing

Atlantis and your Git host need to be able to route and communicate with one another. Your Git host needs to be able to send webhooks to Atlantis and Atlantis needs to be able to make API calls to your Git host.
If you're using
a public Git host like github.com, gitlab.com, gitea.com, bitbucket.org, or dev.azure.com then you'll need to
expose Atlantis to the internet.

If you're using a private Git host like GitHub Enterprise, GitLab Enterprise, self-hosted Gitea or
Bitbucket Server, then Atlantis needs to be routable from the private host and Atlantis will need to be able to route to the private host.

### Data

Atlantis has no external database. With the [Helm chart](#kubernetes-helm-chart), it keeps locks and pull request status in the Kubernetes API, and Terraform plan files on each replica's volume.
If a plan file is lost in between a `plan` and `apply` cycle, then users will have
to re-run `plan`. To keep plans available to every replica, configure a shared plan store (see [`--enable-external-stores`](server-configuration.md#enable-external-stores)).

## Deployment

Atlantis is installed with its [Helm chart](#kubernetes-helm-chart). The
server refuses to start outside Kubernetes, so upstream Atlantis's other
deployment options (a single container, a VM, AWS Fargate, Azure Container
Instances) don't apply to this fork.

* [Kubernetes Helm Chart](#kubernetes-helm-chart)
* [Plain manifests](#plain-manifests)
* [OpenShift](#openshift)
* [Managed Kubernetes (GKE, EKS, AKS)](#managed-kubernetes-gke-eks-aks)
* [Docker image](#docker-image)

### Kubernetes Helm Chart

This fork has its own Helm chart, in [`deploy/helm/atlantis`](https://github.com/Kubelantis/katlantis/tree/master/deploy/helm/atlantis) in this repository. It isn't published to a Helm repository, and it isn't the upstream `runatlantis/helm-charts` chart: its values differ, and upstream values don't apply. It runs Atlantis as a StatefulSet that keeps locks and pull request status in the Kubernetes API, so several replicas can run at once. See [Kubernetes-native Atlantis](https://github.com/Kubelantis/katlantis/blob/master/docs/kubernetes-native.md) for what it installs and how it works.

To install:

1. Clone this repository, at the commit you deploy:

    ```bash
    git clone https://github.com/Kubelantis/katlantis.git
    cd katlantis
    ```

1. Choose the image. The chart defaults to `ghcr.io/kubelantis/katlantis` with the tag `v` + the chart's `appVersion`, which exists once that release is published. Until then, set `image.tag` to a tag published from `master`, such as `dev-alpine-<commit>` (see [Image variants](#image-variants)). Or build the image yourself and push it to a registry your cluster can pull from. Upstream Atlantis images don't include this fork's Kubernetes-native code:

    ```bash
    docker build --target alpine -t registry.example.com/atlantis:my-tag .
    docker push registry.example.com/atlantis:my-tag
    ```

1. Create a namespace and a Secret with your [access credentials](access-credentials.md) and [webhook secret](webhook-secrets.md). Its keys become environment variables of the Atlantis server, for example for GitHub:

    ```bash
    kubectl create namespace atlantis
    kubectl -n atlantis create secret generic atlantis-vcs \
      --from-literal=ATLANTIS_GH_USER=my-bot \
      --from-literal=ATLANTIS_GH_TOKEN=... \
      --from-literal=ATLANTIS_GH_WEBHOOK_SECRET=...
    ```

1. Create a `values.yaml` that names the Secret, sets your URL and [repo allowlist](server-configuration.md#repo-allowlist), and, unless you use the chart's default, the image:

    ```yaml
    image:
      repository: registry.example.com/atlantis
      tag: my-tag
    vcsSecretName: atlantis-vcs
    atlantisUrl: https://atlantis.example.com
    repoAllowlist: github.com/example-org/*
    ```

    Every other value is described in the chart's [`values.yaml`](https://github.com/Kubelantis/katlantis/blob/master/deploy/helm/atlantis/values.yaml), for example `ingress`, `extraEnv` for other server flags, `repoConfig` for the [server-side repo config](server-side-repo-config.md), and `externalStores` for a shared plan store.

1. Install the chart:

    ```bash
    helm install atlantis deploy/helm/atlantis -n atlantis -f values.yaml
    ```

Atlantis should be up and running in minutes! See [Next Steps](#next-steps) for
what to do next.

### Plain manifests

To deploy with plain manifests, for example through a GitOps tool that doesn't
render Helm charts, render the chart with your values and apply the result:

```bash
helm template atlantis deploy/helm/atlantis -n atlantis -f values.yaml --include-crds > atlantis.yaml
```

The chart installs more than the Atlantis pods, and all of it is needed: the
`PullStatus` CRD, a Role for Leases and `PullStatus` objects, a cluster token
Secret and a headless Service for the replicas. Upstream Atlantis's example
manifests and Kustomize bundle have none of these, so they don't work with this
fork.

### OpenShift

The chart hasn't been tested on OpenShift. Two settings likely need changing:

* The chart's `podSecurityContext` sets `runAsUser: 100` and `fsGroup: 1000`.
  OpenShift's default `restricted` security context constraint assigns a user
  from the namespace's range instead, and rejects a fixed one. Set both to
  `null` in your values to let OpenShift choose.
* OpenShift's random user has `/` as its home directory. Set `HOME` through
  `extraEnv`:

    ```yaml
    extraEnv:
      - name: HOME
        value: /home/atlantis
    ```

### Managed Kubernetes (GKE, EKS, AKS)

Use the [Helm chart](#kubernetes-helm-chart). To give Atlantis cloud credentials
without static keys, annotate its service account for your cloud's workload
identity (`serviceAccount.annotations`): GKE Workload Identity, EKS IAM roles for
service accounts or Pod Identity, or AKS Workload Identity. On EKS, the same role
can also give access to the S3 plan and log stores (`externalStores`), which use
the default AWS credential chain.

Once you're done, see [Next Steps](#next-steps).

### Docker image

This fork's image is `ghcr.io/kubelantis/katlantis`, built and published from
this repository. Upstream's `ghcr.io/runatlantis/atlantis` images don't include
this fork's Kubernetes-native code.

#### Image variants

Every release is published in four variants. The unsuffixed tag (for example `v0.40.0-k8s` or `latest`) is the Alpine image.

Every push to `master` is published too: as `dev-<variant>` and, pinned to that commit, `dev-<variant>-<commit>` (for example `dev-alpine-1a2b3c4`), plus `dev` for the Alpine image.

| Tag suffix     | Base   | Bundled Terraform and OpenTofu |
|----------------|--------|--------------------------------|
| `-alpine`      | Alpine | yes                            |
| `-debian`      | Debian | yes                            |
| `-alpine-slim` | Alpine | no                             |
| `-debian-slim` | Debian | no                             |

The full images bundle the last few Terraform minor releases and the current OpenTofu release, and `terraform` on `PATH` points at the newest of them.

The slim images ship without either binary, so vulnerability scanners do not report advisories against Terraform or OpenTofu versions you may not even use. Everything else (`conftest`, `git-lfs`, `git`, `curl`, `dumb-init`) is the same as the full image. Atlantis downloads the Terraform version it needs on first use, so the slim image needs to be told which version that is:

* Set [`--default-tf-version`](server-configuration.md#default-tf-version) as a flag, as `ATLANTIS_DEFAULT_TF_VERSION`, or in the server config file. Without it the server refuses to start with `terraform not found in $PATH`. The slim image deliberately sets no default of its own, because an environment variable baked into the image would take precedence over a version pinned in your config file.
* Per-project `terraform_version` in `atlantis.yaml` and `--tf-download-url` work as usual.
* For OpenTofu, set `ATLANTIS_DEFAULT_TF_DISTRIBUTION=opentofu` and give an OpenTofu version as the default.
* If outbound downloads are not allowed from your Atlantis host (`--tf-download=false`), mount or copy the binaries you need into the image instead. See [Customization](#customization) below.

#### Customization

To add tools to the image, for instance a binary your workflows call, build on
it:

1. Create a Dockerfile:

    ```dockerfile
    FROM ghcr.io/kubelantis/katlantis:{version}

    USER root
    COPY my-tool /usr/local/bin/my-tool
    USER atlantis
    ```

    The image runs as the `atlantis` user. Switch to `root` only for the steps that
    need it. Scripts in `/docker-entrypoint.d/` run before the server starts.

1. Build it and push it to a registry your cluster can pull from:

    ```bash
    docker build -t registry.example.com/atlantis-custom:{tag} .
    docker push registry.example.com/atlantis-custom:{tag}
    ```

1. Set `image.repository` and `image.tag` to it in your chart values.

## Next Steps

* To ensure Atlantis is running, load its UI. The chart's Service exposes it on port `80` (the container listens on `4141`). Reach it through your Ingress, or locally with `kubectl -n atlantis port-forward svc/atlantis 4141:80`.
* Now you're ready to add Webhooks to your repos. See [Configuring Webhooks](configuring-webhooks.md).
