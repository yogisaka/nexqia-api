ALTER TABLE core.role ADD COLUMN requires_physician_data boolean NOT NULL DEFAULT false;
-- permission baru untuk user management (karena ini bagian dari 0c)
INSERT INTO core.permission (id, code, description, module) VALUES
    ('00000000-0000-0000-0004-000000000025', 'core.user.manage', 'Kelola pengguna', 'core') ON CONFLICT (code) DO NOTHING;
INSERT INTO core.permission (id, code, description, module) VALUES
    ('00000000-0000-0000-0004-000000000026', 'core.role.manage', 'Kelola role dan permission', 'core') ON CONFLICT (code) DO NOTHING;
