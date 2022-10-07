package loadbalancer

import (
	"bytes"
	"html/template"

	"github.com/pkg/errors"
)

type ConfigData struct {
	ControlPlanePort int
	BackendServers   map[string]string
	EnableStats      bool
}

const configTemplate = `# Created for kubecon
global
  stats socket /var/run/api.sock user haproxy group haproxy mode 660 level admin expose-fd listeners
  log stdout format raw local0 info

defaults
  mode http
  timeout client 10s
  timeout connect 5s
  timeout server 10s
  timeout http-request 10s
  log global

{{ if .EnableStats -}}
frontend stats
  bind *:8404
  stats enable
  stats uri /
  stats refresh 10s
{{- end }}

frontend control-plane
  bind *:{{ .ControlPlanePort }}
  default_backend kube-apiservers

backend kube-apiservers
  # option httpchk GET /healthz
  {{- range $server, $address := .BackendServers}}
  server {{ $server }} {{ $address }} check
  {{- end}}
`

func Config(data *ConfigData) (config string, err error) {
	t, err := template.New("loadbalancer-config").Parse(configTemplate)
	if err != nil {
		return "", errors.Wrap(err, "failed to parse config template")
	}
	// execute the template
	var buff bytes.Buffer
	err = t.Execute(&buff, data)
	if err != nil {
		return "", errors.Wrap(err, "error executing config template")
	}
	return buff.String(), nil
}
