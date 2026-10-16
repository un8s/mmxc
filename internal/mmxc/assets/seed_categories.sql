-- Default category tree stamped into a brand-new database by createDatabase
-- (newdb.go). Top-level categories use PARENTID = -1 (MMEX convention); children
-- reference their parent's CATEGID. IDs are spaced by 10 so each parent has room
-- for children without collisions.
--
-- "Transfer" (id 1) is required: transferCategoryID() resolves transfers to the
-- top-level category named "Transfer". MMEX attaches transactions to *sub*
-- categories, so the rest of the tree is two levels deep.

-- Top-level categories (PARENTID = -1).
INSERT INTO CATEGORY_V1 (CATEGID, CATEGNAME, ACTIVE, PARENTID) VALUES
  (1,   'Transfer',        1, -1),
  (10,  'Auto',            1, -1),
  (20,  'Bills',           1, -1),
  (30,  'Education',       1, -1),
  (40,  'Food',            1, -1),
  (50,  'Gifts & Charity', 1, -1),
  (60,  'Health',          1, -1),
  (70,  'Household',       1, -1),
  (80,  'Income',          1, -1),
  (90,  'Insurance',       1, -1),
  (100, 'Leisure',         1, -1),
  (110, 'Personal Care',   1, -1),
  (120, 'Shopping',        1, -1),
  (130, 'Taxes',           1, -1),
  (140, 'Travel',          1, -1),
  (150, 'Miscellaneous',   1, -1);

-- Subcategories.
INSERT INTO CATEGORY_V1 (CATEGID, CATEGNAME, ACTIVE, PARENTID) VALUES
  (11,  'Fuel',            1, 10),
  (12,  'Maintenance',     1, 10),
  (13,  'Parking',         1, 10),
  (14,  'Registration',    1, 10),

  (21,  'Electricity',     1, 20),
  (22,  'Gas',             1, 20),
  (23,  'Water',           1, 20),
  (24,  'Internet',        1, 20),
  (25,  'Phone',           1, 20),
  (26,  'TV & Streaming',  1, 20),

  (31,  'Tuition',         1, 30),
  (32,  'Books & Supplies',1, 30),
  (33,  'Courses',         1, 30),

  (41,  'Groceries',       1, 40),
  (42,  'Dining Out',      1, 40),
  (43,  'Coffee & Snacks', 1, 40),

  (51,  'Gifts',           1, 50),
  (52,  'Donations',       1, 50),

  (61,  'Doctor',          1, 60),
  (62,  'Dentist',         1, 60),
  (63,  'Pharmacy',        1, 60),
  (64,  'Optical',         1, 60),

  (71,  'Rent & Mortgage', 1, 70),
  (72,  'Furniture',       1, 70),
  (73,  'Supplies',        1, 70),
  (74,  'Repairs',         1, 70),
  (75,  'Garden',          1, 70),

  (81,  'Salary',          1, 80),
  (82,  'Interest',        1, 80),
  (83,  'Dividends',       1, 80),
  (84,  'Refunds',         1, 80),
  (85,  'Other Income',    1, 80),

  (91,  'Auto Insurance',  1, 90),
  (92,  'Health Insurance',1, 90),
  (93,  'Home Insurance',  1, 90),
  (94,  'Life Insurance',  1, 90),

  (101, 'Hobbies',         1, 100),
  (102, 'Entertainment',   1, 100),
  (103, 'Sports & Fitness',1, 100),
  (104, 'Subscriptions',   1, 100),
  (105, 'Vacation',        1, 100),

  (111, 'Haircut',         1, 110),
  (112, 'Cosmetics',       1, 110),

  (121, 'Clothing',        1, 120),
  (122, 'Electronics',     1, 120),
  (123, 'General',         1, 120),

  (131, 'Income Tax',      1, 130),
  (132, 'Property Tax',    1, 130),

  (141, 'Flights',         1, 140),
  (142, 'Lodging',         1, 140),
  (143, 'Local Transport', 1, 140),

  (151, 'Bank Fees',       1, 150),
  (152, 'Cash Withdrawal', 1, 150),
  (153, 'Other',           1, 150);
