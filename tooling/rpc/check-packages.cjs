// Verify installed generator packages against the repository lockfile.
'use strict';

const fs = require('node:fs');
const path = require('node:path');

if (process.argv.length !== 4) {
  throw new Error('Usage: check-packages.cjs LOCKFILE NODE_MODULES');
}

const lock = JSON.parse(fs.readFileSync(process.argv[2], 'utf8'));
const root = lock.packages[''];
for (const [name, expected] of Object.entries({...root.dependencies, ...root.devDependencies})) {
  const installed = path.join(process.argv[3], name, 'package.json');
  const actual = JSON.parse(fs.readFileSync(installed, 'utf8')).version;
  if (actual !== expected) {
    throw new Error(`${name}: expected ${expected}, got ${actual}; run scripts/setup.sh`);
  }
}
