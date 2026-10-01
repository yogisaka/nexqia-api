-- Owner yang self-register (0a, POST /auth/register) gak pernah diminta
-- gender-nya (form registrasi sengaja pendek), tapi core.person.gender NOT
-- NULL memaksa handler hardcode "male" sebagai placeholder — data salah buat
-- Owner yang bukan laki-laki. Dijadiin nullable: registrasi patient tetap
-- wajib isi gender (dienforce di HTTP layer, createPersonRequest.Gender
-- `binding:"required"`, internal/server/person.go), cuma Owner-bootstrap
-- yang sekarang boleh NULL. Lihat 
-- 2026-09-24-fix-0a-0b-0c-review-findings.md Task 3.
ALTER TABLE core.person ALTER COLUMN gender DROP NOT NULL;
