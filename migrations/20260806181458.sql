-- Create "audit_system_events" table
CREATE TABLE "public"."audit_system_events" (
  "id" text NOT NULL,
  "type" text NOT NULL,
  "metadata" jsonb NULL,
  "created_at" timestamptz NULL,
  "updated_at" timestamptz NULL,
  CONSTRAINT "uni_audit_system_events_id" PRIMARY KEY ("id")
);
-- Create "workspaces" table
CREATE TABLE "public"."workspaces" (
  "id" text NOT NULL,
  "name" text NOT NULL,
  "description" text NULL,
  "volume_name" text NOT NULL,
  "volume_state" text NOT NULL DEFAULT 'NONE',
  "volume_metadata" jsonb NULL,
  "created_at" timestamptz NULL,
  "updated_at" timestamptz NULL,
  PRIMARY KEY ("id"),
  CONSTRAINT "uni_workspaces_name" UNIQUE ("name"),
  CONSTRAINT "uni_workspaces_volume_name" UNIQUE ("volume_name")
);
-- Create "artifacts" table
CREATE TABLE "public"."artifacts" (
  "id" text NOT NULL,
  "workspace_id" text NOT NULL,
  "name" text NOT NULL,
  "description" text NULL,
  "object_key" text NOT NULL,
  "mime_type" text NOT NULL,
  "size" bigint NOT NULL,
  "state" text NOT NULL DEFAULT 'RECORDED',
  "created_at" timestamptz NULL,
  "updated_at" timestamptz NULL,
  PRIMARY KEY ("id"),
  CONSTRAINT "fk_artifacts_workspace" FOREIGN KEY ("workspace_id") REFERENCES "public"."workspaces" ("id") ON UPDATE NO ACTION ON DELETE CASCADE
);
-- Create index "artifact_workspace_name" to table: "artifacts"
CREATE UNIQUE INDEX "artifact_workspace_name" ON "public"."artifacts" ("workspace_id", "name");
