// Used only by lifecycle_test.py; never substitutes for either real suite.
'use strict';
console.error('Synthetic private failure: postgres://fixture:secret@127.0.0.1/fry_test');
process.exitCode = 7;
