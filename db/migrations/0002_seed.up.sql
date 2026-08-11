-- デモ商品を1件シード。すでに存在すれば何もしない。
INSERT INTO products (id, name, price_jpy, tokusho_snapshot)
VALUES ('01HZZZZZZZZZZZZZZZZZZZZZZZ', 'デモ商品', 1000, '{"seller":"kessai demo"}')
ON CONFLICT (id) DO NOTHING;
