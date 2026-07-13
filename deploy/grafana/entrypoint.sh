#!/bin/sh

# Grafana 文件 provisioning 不允许联系点的 URL 为空。群 webhook 是可选的，
# 因此先把受 git 跟踪的基础配置复制到临时目录，再只为非空 env 生成 receiver。
# 生成文件仍保留 $__env{VAR} 占位，不把 secret 写入磁盘或日志。
set -eu

src=/etc/grafana/provisioning-source
dst=/tmp/pandora-grafana-provisioning

rm -rf "$dst"
mkdir -p "$dst"
cp -R "$src"/. "$dst"/
GF_PATHS_PROVISIONING=$dst
export GF_PATHS_PROVISIONING

# 绕过 compose 直接运行容器时也保证 ntfy 联系点有合法 URL。
if [ -z "${PANDORA_ALERT_NTFY_URL:-}" ]; then
  PANDORA_ALERT_NTFY_URL='http://ntfy:80/pandora-alerts?tpl=1&t=%7B%7B.title%7D%7D&m=%7B%7B.message%7D%7D'
  export PANDORA_ALERT_NTFY_URL
fi

has_group=false
if [ -n "${PANDORA_ALERT_WECOM_URL:-}" ] || \
   [ -n "${PANDORA_ALERT_FEISHU_WEBHOOK:-}" ]; then
  has_group=true
fi

if [ "$has_group" = true ]; then
  contact_points="$dst/alerting/contact-points.yaml"
  policies="$dst/alerting/notification-policies.yaml"

  cat >> "$contact_points" <<'YAML'

  # 可选群渠道只在对应 env 非空时由本 entrypoint 动态追加。
  - orgId: 1
    name: group-ops
    receivers:
YAML

  if [ -n "${PANDORA_ALERT_WECOM_URL:-}" ]; then
    cat >> "$contact_points" <<'YAML'
      - uid: pandora-wecom-group
        type: wecom
        settings:
          url: $__env{PANDORA_ALERT_WECOM_URL}
          msgtype: markdown
          title: '{{ template "pandora.title" . }}'
          message: '{{ template "pandora.message" . }}'
        disableResolveMessage: false
YAML
  fi

  if [ -n "${PANDORA_ALERT_FEISHU_WEBHOOK:-}" ]; then
    cat >> "$contact_points" <<'YAML'
      - uid: pandora-feishu-group
        type: webhook
        settings:
          url: $__env{PANDORA_ALERT_FEISHU_WEBHOOK}
          httpMethod: POST
          title: '{{ template "pandora.title" . }}'
          message: '{{ template "pandora.message" . }}'
        disableResolveMessage: false
YAML
  fi

  # 有群渠道:warning 只发 group-ops；critical 先发 ntfy，continue 后再发 group-ops。
  cat > "$policies" <<'YAML'
apiVersion: 1

policies:
  - orgId: 1
    receiver: group-ops
    group_by: ['alertname', 'fleet']
    group_wait: 30s
    group_interval: 5m
    repeat_interval: 4h
    routes:
      - receiver: ntfy-personal
        matchers:
          - severity = critical
        group_by: ['alertname', 'fleet']
        group_wait: 10s
        group_interval: 5m
        repeat_interval: 1h
        continue: true
      - receiver: group-ops
        matchers:
          - severity = critical
        group_wait: 10s
        repeat_interval: 1h
      - receiver: group-ops
        matchers:
          - severity = warning
        repeat_interval: 4h
YAML
fi

exec /run.sh "$@"
