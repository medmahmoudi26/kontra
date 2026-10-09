# canary · {{ run.status }}

{% if run.status == "running" %}
Running for {{ run.duration_s | duration }}. This page re-renders while the run goes, and the final
report replaces it when the run ends.

{% if run.progress %}
**{{ run.progress.units_done }} of {{ run.progress.units_total }}** target(s) swept so far{% if run.progress.isolated > 0 %}, {{ run.progress.isolated }} dropped{% endif %}.
{% else %}
No target has reached a Worker yet: the Fleet is still coming up.
{% endif %}
{% assign flying = datasets["in flight"] %}
{% if flying %}
## Records so far: {{ flying.rows }}

The newest ones, as the actor pushed them:

| target | step | phase | latency (ms) | worker |
|---|---|---|---|---|
{% for r in flying.tail -%}
| {{ r.target }} | {{ r.step }} | {{ r.phase }} | {{ r.latency_ms }} | {{ r.worker }} |
{% endfor %}
{% endif %}
{% elsif result %}
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
