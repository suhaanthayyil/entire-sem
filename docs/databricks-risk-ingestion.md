# Databricks risk intelligence

This optional [Databricks Declarative Automation Bundle](https://docs.databricks.com/aws/en/dev-tools/bundles/) turns the existing `entire graph risk --format json` output into a governed, queryable Delta table. It is deliberately separate from the local Go CLI: running or not running Databricks does not change risk analysis results.

## What it stores

The job writes one row per changed entity to Unity Catalog. Each row includes the report's revision/checkpoint, changeset and entity risk, changed file and line, dependent count, graph-evidence JSON, recommended-test JSON, inference, limitations, and ingestion timestamp.

## Deploy

1. Authenticate the [Databricks CLI](https://docs.databricks.com/aws/en/dev-tools/cli/authentication.html) to a workspace that has Unity Catalog and permission to create a schema/table in the chosen catalog.
2. Produce a report and place it in a Unity Catalog volume or other Spark-readable location:

   ```sh
   entire graph risk --repo . --base main --head HEAD --format json > risk-report.json
   # Upload risk-report.json to, for example:
   # /Volumes/main/entire_graph/risk_reports/risk-report.json
   ```

3. From `databricks/`, validate and deploy the bundle:

   ```sh
   databricks bundle validate -t dev
   databricks bundle deploy -t dev
   databricks bundle run entire_graph_risk_ingestion -t dev \
     --params risk_report_path=/Volumes/main/entire_graph/risk_reports/risk-report.json
   ```

   If you use another catalog, schema, or table name, set them during deploy/run with the corresponding bundle variables.

4. Query the result:

   ```sql
   SELECT overall_risk, entity_risk, change_type, entity_name, file_path,
          dependents_count, graph_evidence_json, affected_tests_json, ingested_at
   FROM main.entire_graph.changeset_risk
   ORDER BY ingested_at DESC, entity_risk DESC, dependents_count DESC;
   ```

## Operational notes

- The report must use the current `riskReport` JSON contract (format version 1).
- The ingestion is append-only. Re-running the same report intentionally records another ingestion event; use revision/checkpoint and `source_report_path` for deduplication in downstream queries.
- This repository contains a deployable Databricks integration, but it is not a claim that a job has already been deployed or run in a workspace. Record the workspace job URL/run result in the submission only after completing the deploy step.
