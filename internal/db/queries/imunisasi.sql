-- name: ListProgramImmunizationSchedule :many
SELECT * FROM ref.immunization_schedule
WHERE source_version = 'kia-2024-kmk35-2025'
ORDER BY antigen_slug, dosis_ke;

-- name: ExistsImmunizationScheduleDosis :one
SELECT EXISTS (
  SELECT 1 FROM ref.immunization_schedule
  WHERE antigen_slug = $1 AND dosis_ke = $2
) AS ok;
