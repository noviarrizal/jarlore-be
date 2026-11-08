-- +goose Up
-- System reference data. typicalDays are references for the UI, never targets or verdicts.

-- At most one bottle per batch is marked as the best result of that split.
CREATE UNIQUE INDEX bottles_one_reference_per_batch ON bottles (batch_id) WHERE is_reference;

INSERT INTO fermentation_types (id, name, code_prefix, stages, default_volume_ml, default_ingredients, default_params, sort_order) VALUES
('kombucha', 'Kombucha', 'KB',
 '[{"key":"f1","label":"F1 in jar","typicalDays":[7,14]},{"key":"f2","label":"F2 in bottles","typicalDays":[2,5]}]',
 4200,
 '[{"name":"Filtered water","role":"water","amount":3.8,"unit":"L"},
   {"name":"Black tea","role":"tea","amount":12,"unit":"g"},
   {"name":"Cane sugar","role":"sugar","amount":200,"unit":"g"},
   {"name":"Starter tea","role":"starter","amount":400,"unit":"ml"}]',
 '{"targetF1Days":10}', 1),
('water_kefir', 'Water kefir', 'WK',
 '[{"key":"f1","label":"F1 with grains","typicalDays":[1,3]},{"key":"f2","label":"F2 in bottles","typicalDays":[1,3]}]',
 1000,
 '[{"name":"Filtered water","role":"water","amount":1,"unit":"L"},
   {"name":"Water kefir grains","role":"starter","amount":60,"unit":"g"},
   {"name":"Cane sugar","role":"sugar","amount":60,"unit":"g"}]',
 '{"targetF1Days":2}', 2),
('custom', 'Custom ferment', 'CU',
 '[{"key":"ferment","label":"Fermenting","typicalDays":[5,30]}]',
 1000, '[]', '{}', 3);

INSERT INTO measurement_definitions (user_id, key, label, kind, quantity, unit, decimals, min_value, max_value, requires_method) VALUES
(NULL, 'ph',   'pH',      'numeric', NULL,          'pH', 1, 2,    7,   true),
(NULL, 'temp', 'Temp',    'numeric', 'temperature', '°C', 1, -10,  60,  false),
(NULL, 'brix', 'Brix',    'numeric', NULL,          '°Bx', 1, 0,   40,  false),
(NULL, 'sg',   'Gravity', 'numeric', NULL,          'SG', 3, 0.98, 1.2, false);
