#!/usr/bin/env bash
# 安装/升级 Agones 到最新稳定版（本仓库基线 = v1.58.0）。
# 官方仓库已从 googleforgames/agones 迁到 agones-dev/agones；helm repo 地址不变。
#
# 用法：
#   ./deploy/ds/install-agones.sh [VERSION]
#   VERSION 默认 1.58.0（撰写时最新稳定版）。升级时把这里和 Fleet 一起过一遍。
#
# 前置：kubectl 指向目标集群、已装 helm 3。
# 本仓库启动/压测基线：Agones v1.58.0 + Kubernetes v1.35.1。
set -euo pipefail

VERSION="${1:-1.58.0}"
NAMESPACE="agones-system"
INSTALL_YAML_URL="https://raw.githubusercontent.com/agones-dev/agones/v${VERSION}/install/yaml/install.yaml"

echo "[install-agones] 目标版本: ${VERSION}"

if helm repo add agones https://agones.dev/chart/stable 2>/dev/null || true; then
  :
fi

# 国内网络下 agones.dev/chart/stable 会跳到 Google Storage，可能不可达。
# 先走 Helm；失败后自动 fallback 到官方 release install.yaml。
if helm repo update && helm upgrade --install agones agones/agones \
    --version "${VERSION}" \
    --namespace "${NAMESPACE}" \
    --create-namespace \
    --wait; then
  echo "[install-agones] Helm 安装完成。"
else
  echo "[install-agones] Helm repo 不可用，fallback 到 install.yaml: ${INSTALL_YAML_URL}" >&2
  kubectl create namespace "${NAMESPACE}" --dry-run=client -o yaml | kubectl apply -f -
  # Agones v1.58 CRD 较大，普通 client-side apply 会触发 262KB annotation 限制。
  # server-side apply 可避开；force-conflicts 用于清理前一次 client-side 半安装留下的 field manager。
  kubectl apply --server-side --force-conflicts -f "${INSTALL_YAML_URL}"
  kubectl -n "${NAMESPACE}" rollout status deployment/agones-controller --timeout=10m
  kubectl -n "${NAMESPACE}" rollout status deployment/agones-extensions --timeout=10m
  kubectl -n "${NAMESPACE}" rollout status deployment/agones-allocator --timeout=10m || true
fi

echo "[install-agones] 校验 CRD 与控制器："
kubectl get crd | grep agones.dev || true
kubectl -n "${NAMESPACE}" get pods

echo "[install-agones] 完成。接着 apply deploy/k8s/agones 下的 Fleet/RBAC。"
