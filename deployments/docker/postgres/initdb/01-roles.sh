#!/bin/sh
# Bootstraps EACP roles and database on first container start.
#
#   eacp_owner  owns the schema and runs migrations
#   eacp_app    used by services: NOSUPERUSER NOBYPASSRLS, so Row-Level
#               Security always applies (ADR-021). Services refuse to start
#               if their role can bypass RLS (storage.CheckRoleSafety).
#
# Passwords come from the environment. The compose defaults are for local
# development only.
set -eu

psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname postgres \
  -v owner_pw="$EACP_OWNER_PASSWORD" -v app_pw="$EACP_APP_PASSWORD" <<'SQL'
CREATE ROLE eacp_owner LOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE PASSWORD :'owner_pw';
CREATE ROLE eacp_app   LOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE PASSWORD :'app_pw';
CREATE DATABASE eacp OWNER eacp_owner;
SQL
