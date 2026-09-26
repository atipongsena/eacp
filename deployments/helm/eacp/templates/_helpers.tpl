{{- define "eacp.name" -}}{{ .Release.Name }}{{- end -}}

{{- define "eacp.labels" -}}
app.kubernetes.io/name: eacp
app.kubernetes.io/instance: {{ .root.Release.Name }}
app.kubernetes.io/version: {{ .root.Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .root.Release.Service }}
app.kubernetes.io/component: {{ .component }}
{{- end -}}

{{- define "eacp.selector" -}}
app.kubernetes.io/name: eacp
app.kubernetes.io/instance: {{ .root.Release.Name }}
app.kubernetes.io/component: {{ .component }}
{{- end -}}

{{- define "eacp.image" -}}{{ .Values.image.repository }}:{{ .Values.image.tag | default .Chart.AppVersion }}{{- end -}}
{{- define "eacp.pdpImage" -}}{{ .Values.pdp.image.repository }}:{{ .Values.pdp.image.tag | default .Chart.AppVersion }}{{- end -}}

{{- /* Pod-level hardening. A pre-install hook runs before the chart's
     ServiceAccount exists, so it passes sa=false and uses the default one
     (whose token is not mounted either). */ -}}
{{- define "eacp.podSecurity" -}}
automountServiceAccountToken: false
enableServiceLinks: false
{{- if .sa }}
serviceAccountName: {{ .root.Release.Name }}
{{- end }}
securityContext:
  runAsNonRoot: true
  runAsUser: 65532
  runAsGroup: 65532
  fsGroup: 65532
  seccompProfile:
    type: RuntimeDefault
{{- end -}}

{{- define "eacp.containerSecurity" -}}
securityContext:
  allowPrivilegeEscalation: false
  readOnlyRootFilesystem: true
  capabilities:
    drop: [ALL]
{{- end -}}

{{- define "eacp.spread" -}}
topologySpreadConstraints:
  - maxSkew: 1
    topologyKey: kubernetes.io/hostname
    whenUnsatisfiable: ScheduleAnyway
    labelSelector:
      matchLabels:
        {{- include "eacp.selector" . | nindent 8 }}
{{- end -}}

{{- /* Extra env from a values map; the validation template refuses secrets. */ -}}
{{- define "eacp.extraEnv" -}}
{{- range $k, $v := . }}
- name: {{ $k }}
  value: {{ $v | quote }}
{{- end }}
{{- end -}}

{{- define "eacp.shutdownEnv" -}}
- name: EACP_SHUTDOWN_DELAY
  value: {{ .Values.shutdown.delay | quote }}
- name: EACP_SHUTDOWN_TIMEOUT
  value: {{ .Values.shutdown.timeout | quote }}
{{- end -}}

{{- define "eacp.seconds" -}}{{ trimSuffix "s" . | int }}{{- end -}}
