{{- define "leartech.routing.host" -}}
{{- $name := .Values.service.name | default (include "leartech.fullname" .) -}}
{{- printf "%s%s%s" $name .Values.jxRequirements.ingress.namespaceSubDomain .Values.jxRequirements.ingress.domain -}}
{{- end -}}

{{- define "leartech.ingress" -}}
{{- if and (.Values.jxRequirements.ingress.domain) (not .Values.knativeDeploy) }}
{{- $ingress := .Values.ingress | default dict -}}
{{- $gw := .Values.gateway | default dict -}}
{{- $ingressEnabled := true -}}
{{- if hasKey $ingress "enabled" }}{{- $ingressEnabled = $ingress.enabled -}}{{- end -}}
{{- $global := .Values.global | default dict -}}
{{- $gwEnabled := false -}}
{{- if hasKey $global "gatewayAPIEnabled" }}{{- $gwEnabled = $global.gatewayAPIEnabled -}}{{- end -}}
{{- if hasKey $gw "enabled" }}{{- if $gw.enabled }}{{- $gwEnabled = true -}}{{- end -}}{{- end -}}
{{- if not $ingressEnabled -}}
{{- else if $gwEnabled -}}
{{ include "leartech.httproute" . }}
{{- else -}}
{{- $host := include "leartech.routing.host" . -}}
{{- $backendName := include "leartech.fullname" . -}}
{{- $svcPort := .Values.service.externalPort | default 8080 -}}
{{- $annotations := dict -}}
{{- $_ := merge $annotations ($ingress.annotations | default dict) (.Values.jxRequirements.ingress.annotations | default dict) -}}
{{- $class := $ingress.className | default $ingress.classAnnotation | default "nginx" -}}
{{- if not (hasKey $annotations "kubernetes.io/ingress.class") }}
{{- $_ := set $annotations "kubernetes.io/ingress.class" $class }}
{{- end }}
apiVersion: {{ .Values.jxRequirements.ingress.apiVersion | default "networking.k8s.io/v1" }}
kind: Ingress
metadata:
  name: {{ .Values.service.name | default (include "leartech.fullname" .) }}
  labels:
    {{- include "leartech.labels" . | nindent 4 }}
    {{- with $ingress.labels }}
    {{- toYaml . | nindent 4 }}
    {{- end }}
  {{- if $annotations }}
  annotations:
    {{- toYaml $annotations | nindent 4 }}
  {{- end }}
spec:
  ingressClassName: {{ $class }}
  rules:
  - host: {{ $host }}
    http:
      paths:
      - path: /
        pathType: {{ $ingress.pathType | default "ImplementationSpecific" }}
        backend:
          service:
            name: {{ $backendName }}
            port:
              number: {{ $svcPort }}
{{- if .Values.jxRequirements.ingress.tls.enabled }}
  tls:
  - hosts:
    - {{ $host }}
{{- if .Values.jxRequirements.ingress.tls.production }}
    secretName: "tls-{{ .Values.jxRequirements.ingress.domain | replace "." "-" }}-p"
{{- else }}
    secretName: "tls-{{ .Values.jxRequirements.ingress.domain | replace "." "-" }}-s"
{{- end }}
{{- end }}
{{- end }}
{{- end }}
{{- end -}}

{{- define "leartech.httproute" -}}
{{- $gw := .Values.gateway | default dict -}}
{{- $host := include "leartech.routing.host" . -}}
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: {{ .Values.service.name | default (include "leartech.fullname" .) }}
  labels:
    {{- include "leartech.labels" . | nindent 4 }}
    {{- with (.Values.ingress | default dict).labels }}
    {{- toYaml . | nindent 4 }}
    {{- end }}
spec:
  parentRefs:
  - name: {{ $gw.name | default "envoy-gateway" }}
    namespace: {{ $gw.namespace | default "envoy-gateway-system" }}
    sectionName: {{ $gw.sectionName | default "https" }}
  hostnames:
  - {{ $host | quote }}
  rules:
  - matches:
    - path:
        type: PathPrefix
        value: "/"
{{- with (.Values.ingress | default dict).removeRequestHeaders }}
    filters:
    - type: RequestHeaderModifier
      requestHeaderModifier:
        remove:
{{- range . }}
        - {{ . }}
{{- end }}
{{- end }}
    backendRefs:
    - name: {{ include "leartech.fullname" . }}
      port: {{ .Values.service.externalPort | default 8080 }}
{{- end -}}
