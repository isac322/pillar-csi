{{- define "type_members" -}}
{{- $field := . -}}
{{- if eq $field.Name "metadata" -}}
Refer to Kubernetes API documentation for fields of `metadata`.
{{- else -}}
{{ template "cellDoc" $field.Doc }}
{{- end -}}
{{- end -}}

{{- /*
proseDoc prepares Go doc text for Markdown:
"<" becomes "&lt;" so placeholders such as <pool> are not parsed as HTML, and
CLI flags (--config) are wrapped in backticks so typographic replacement does
not turn "--" into a dash.
*/ -}}
{{- define "proseDoc" -}}
{{- regexReplaceAll "(^|[\\s(])(--[a-zA-Z][a-zA-Z0-9-]*)" (. | replace "<" "&lt;") "${1}`${2}`" -}}
{{- end -}}

{{- /* cellDoc is proseDoc for a table cell: one line, with "- " list items shown as bullets. */ -}}
{{- define "cellDoc" -}}
{{- $s := regexReplaceAll "(^|[\\s(])(--[a-zA-Z][a-zA-Z0-9-]*)" (. | replace "<" "&lt;") "${1}`${2}`" -}}
{{- regexReplaceAll "<br />\\s*- " (markdownRenderFieldDoc $s) "<br />• " -}}
{{- end -}}
