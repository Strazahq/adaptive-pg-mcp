-- The helpdesk database of the compose trial, with two logins.
-- helpdesk_read may only read public.tickets. helpdesk_write may also
-- insert, update and delete its rows. Neither may create tables.

CREATE TABLE public.tickets (
  id         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  subject    text NOT NULL,
  status     text NOT NULL DEFAULT 'open' CHECK (status IN ('open', 'pending', 'closed')),
  opened_on  date NOT NULL DEFAULT current_date,
  updated_on date NOT NULL DEFAULT current_date
);

INSERT INTO public.tickets (subject, status, opened_on, updated_on) VALUES
  ('VPN drops every hour',          'open',    current_date - 2,  current_date - 1),
  ('Printer on floor 2 is offline', 'open',    current_date - 5,  current_date - 5),
  ('Reset password for a new hire', 'closed',  current_date - 9,  current_date - 8),
  ('Laptop battery swells',         'pending', current_date - 40, current_date - 30),
  ('Shared drive is read-only',     'pending', current_date - 20, current_date - 12),
  ('Badge does not open door 3',    'pending', current_date - 3,  current_date - 2);

CREATE ROLE helpdesk_read LOGIN PASSWORD 'demo';
CREATE ROLE helpdesk_write LOGIN PASSWORD 'demo';
GRANT SELECT ON public.tickets TO helpdesk_read;
GRANT SELECT, INSERT, UPDATE, DELETE ON public.tickets TO helpdesk_write;
