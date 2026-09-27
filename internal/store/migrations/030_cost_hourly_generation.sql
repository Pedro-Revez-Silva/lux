-- Older cost writers leave this generation at zero when replacing a source.
ALTER TABLE cost_sources ADD COLUMN hourly_generation bigint NOT NULL DEFAULT 0;

CREATE FUNCTION cost_source_hourly_generation() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.hourly_generation = OLD.hourly_generation THEN
    NEW.hourly_generation := 0;
  END IF;
  RETURN NEW;
END;
$$;
CREATE TRIGGER cost_source_hourly_generation BEFORE UPDATE ON cost_sources
  FOR EACH ROW EXECUTE FUNCTION cost_source_hourly_generation();
