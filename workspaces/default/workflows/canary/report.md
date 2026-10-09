# canary · {{ run.status }}

{% if result %}
{{ result.summary }}

| fleet | sweep | records | took |
|---|---|---|---|
| {{ result.machines }} {{ result.provider }} machine(s), {{ result.sessions }} session(s) each | {{ result.targets | size }} target(s) × {{ result.steps }} step(s), one every {{ result.every }}s | {{ result.records }} of {{ result.expected }} | {{ run.duration_s | duration }} |

## Targets

| target | records | outcome |
|---|---|---|
{% for t in result.targets -%}
| {{ t.target }} | {{ t.records }} | {{ t.outcome }}{% if t.error %}: {{ t.error }}{% endif %} |
{% endfor %}
{% if result.voided %}
## Why the sweep was voided

Nothing is known about any target, because the Batch never reported back.

{% code "text", result.voided %}
{% endif %}
## Read the rows

The rows are in the **{{ result.dataset }}** Dataset. This query reads back exactly this run's:

{% code "sql", result.query %}
{% else %}
This run ended **{{ run.status }}** after {{ run.duration_s | duration }} and returned nothing, so
there is no sweep to report. The Fleet scope releases its Machines on the way out either way.

{% if run.error %}
{% code "text", run.error.message %}
{% endif %}
{% endif %}
