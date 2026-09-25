-- Remove the 4 legacy duplicates of core.* codes and the unused procurement
-- code (decision 2026-09-25). No code checks any of them. Roles holding a
-- duplicate first receive its core.* equivalent so nobody loses access.
INSERT INTO core.role_permission (role_id, permission_id)
SELECT rp.role_id, core_p.id
FROM core.role_permission rp
JOIN core.permission dup ON dup.id = rp.permission_id
JOIN core.permission core_p ON core_p.code = CASE dup.code
    WHEN 'master.person.manage'       THEN 'core.person.manage'
    WHEN 'master.service_item.manage' THEN 'core.tariff.manage'
    WHEN 'rbac.user.manage'           THEN 'core.user.manage'
    WHEN 'rbac.role.manage'           THEN 'core.role.manage'
END
WHERE dup.code IN ('master.person.manage', 'master.service_item.manage', 'rbac.user.manage', 'rbac.role.manage')
ON CONFLICT DO NOTHING;

DELETE FROM core.role_permission
WHERE permission_id IN (
    SELECT id FROM core.permission
    WHERE code IN ('master.person.manage', 'master.service_item.manage', 'rbac.user.manage', 'rbac.role.manage', 'procurement.po.create')
);

DELETE FROM core.permission
WHERE code IN ('master.person.manage', 'master.service_item.manage', 'rbac.user.manage', 'rbac.role.manage', 'procurement.po.create');