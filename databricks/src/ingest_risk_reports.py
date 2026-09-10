# Databricks notebook source
# MAGIC %md
# MAGIC # Entire Graph risk-report ingestion
# MAGIC
# MAGIC This job consumes JSON produced by `entire graph risk --format json`,
# MAGIC normalizes one row per changed entity, and appends the result to Delta.

# COMMAND ----------

from pyspark.sql import functions as F


def widget(name: str) -> str:
    dbutils.widgets.text(name, "")
    return dbutils.widgets.get(name).strip()


def identifier(value: str, label: str) -> str:
    # Catalog, schema, and table names are used in SQL. Keep them simple and
    # quoted so job parameters cannot change the statement's structure.
    if not value or not value.replace("_", "a").replace("-", "a").isalnum():
        raise ValueError(f"{label} must contain only letters, digits, '_' or '-'")
    return f"`{value}`"


catalog = widget("catalog")
schema = widget("schema")
target_table = widget("target_table")
risk_report_path = widget("risk_report_path")

if not risk_report_path:
    raise ValueError("risk_report_path is required")

qualified_table = ".".join(
    [identifier(catalog, "catalog"), identifier(schema, "schema"), identifier(target_table, "target_table")]
)
spark.sql(f"CREATE SCHEMA IF NOT EXISTS {identifier(catalog, 'catalog')}.{identifier(schema, 'schema')}")

report = spark.read.option("multiline", "true").json(risk_report_path)
if report.rdd.isEmpty():
    raise ValueError(f"risk report is empty: {risk_report_path}")

records = (
    report.select(
        F.col("format_version").alias("report_format_version"),
        F.col("base").alias("base_revision"),
        F.col("head").alias("head_revision"),
        F.col("checkpoint"),
        F.col("overall_risk"),
        F.col("entities_changed"),
        F.col("entities_analyzed"),
        F.col("entities_skipped"),
        F.explode_outer("entries").alias("entry"),
    )
    .where(F.col("entry").isNotNull())
    .select(
        "report_format_version",
        "base_revision",
        "head_revision",
        "checkpoint",
        "overall_risk",
        "entities_changed",
        "entities_analyzed",
        "entities_skipped",
        F.col("entry.name").alias("entity_name"),
        F.col("entry.kind").alias("entity_kind"),
        F.col("entry.change_type").alias("change_type"),
        F.col("entry.file_path").alias("file_path"),
        F.col("entry.start_line").alias("start_line"),
        F.col("entry.dependents_count").alias("dependents_count"),
        F.col("entry.level").alias("entity_risk"),
        F.to_json(F.col("entry.graph_evidence")).alias("graph_evidence_json"),
        F.to_json(F.col("entry.affected_tests")).alias("affected_tests_json"),
        F.col("entry.inference").alias("inference"),
        F.to_json(F.col("entry.limitations")).alias("limitations_json"),
        F.lit(risk_report_path).alias("source_report_path"),
        F.current_timestamp().alias("ingested_at"),
    )
)

records.write.format("delta").mode("append").option("mergeSchema", "true").saveAsTable(qualified_table)
display(records.orderBy(F.col("entity_risk").desc(), F.col("dependents_count").desc()))
