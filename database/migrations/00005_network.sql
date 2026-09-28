-- Feature 6, the full network: a camera may start although its address
-- answers on the LAN when forced (D14, off by default), and the node keeps
-- the address a camera really holds with where it came from: its static
-- configuration, a DHCP lease, or the profile's factory address when DHCP
-- failed (D24).

-- +goose Up

ALTER TABLE camera_network ADD COLUMN force INTEGER NOT NULL DEFAULT 0 CHECK (force IN (0, 1));
ALTER TABLE camera_status ADD COLUMN ip TEXT NOT NULL DEFAULT '';
ALTER TABLE camera_status ADD COLUMN ip_source TEXT NOT NULL DEFAULT '' CHECK (ip_source IN ('', 'static', 'dhcp', 'factory'));

-- +goose Down

ALTER TABLE camera_status DROP COLUMN ip_source;
ALTER TABLE camera_status DROP COLUMN ip;
ALTER TABLE camera_network DROP COLUMN force;
