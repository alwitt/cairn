-- Modify "artifacts" table
ALTER TABLE "public"."artifacts" ADD COLUMN "revision" bigint NOT NULL DEFAULT 1;
