-- REF: the regions of a country programme (S5R-D15(a), simulated sign-off E0 of 2026-11-13; UL
-- PLATFORM-CountryProgrammeVersion; CNX-INV-001 finding region_review).
--
-- A programme may list ISO 3166-2 subdivision codes of some of its countries: it then affects only
-- those regions of the countries they belong to, and a country of the programme without a listed
-- region is affected whole. Reference data admits a region only of one of the programme's countries
-- and of an officially assigned country code (refapi.ValidRegionCode); the column checks the
-- canonical form: two-letter country, hyphen, one to three letters or digits, each once, in
-- ascending order. NULL is none, as for every programme imported before this migration: the column
-- is added without a default, no row is rewritten, and a programme without regions keeps its content
-- hash (the canonical JSON leaves an empty region list out). The immutability trigger of 000002
-- covers the new column; brant_app's table grants cover it.

CREATE FUNCTION brant_ref.canonical_region_codes(p_codes text[]) RETURNS boolean
    LANGUAGE sql IMMUTABLE STRICT SET search_path = pg_catalog, pg_temp
AS $$
    SELECT cardinality(p_codes) BETWEEN 1 AND 1000
       AND pg_catalog.array_to_string(p_codes, ',') ~ '^[A-Z]{2}-[A-Z0-9]{1,3}(,[A-Z]{2}-[A-Z0-9]{1,3})*$'
       AND p_codes = ARRAY(SELECT DISTINCT c COLLATE "C" FROM pg_catalog.unnest(p_codes) AS u(c) ORDER BY 1)
$$;

ALTER TABLE brant_ref.country_programme ADD COLUMN regions text[] CHECK (brant_ref.canonical_region_codes(regions));
