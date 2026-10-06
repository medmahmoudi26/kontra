# {{ workflow.name }} over {{ input.catalog }}

{% if result %}
{{ result.summary }}

| products | missing image | isolated |
|---|---|---|
| {{ result.products }} | {{ result.missing_image }} | {{ result.isolated }} |

## Largest price changes

| sku | old | new |
|---|---|---|
{% for p in result.price_changes -%}
| {{ p.sku }} | {{ p.old }} | {{ p.new }} |
{% endfor %}
{% if result.sample_request %}
## Sample request

{% code "http", result.sample_request %}
{% endif %}
{% else %}
This run ended **{{ run.status }}** after {{ run.duration_s }}s.

{{ run.error.message }}
{% endif %}
