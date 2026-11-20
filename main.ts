// Entry point for the github-core-clone-01 forge.
//
// The forge itself is a statically linked Go binary (vendored beside this
// file as ./forge, sources in ./src). This launcher only starts it as one
// process and mirrors its exit code: the binary binds its own ephemeral
// port, registers it with the suite, serves git smart HTTP + REST v3 +
// GraphQL + MCP, polls /done and exits 0 when the suite is over.
import { spawn } from "node:child_process";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";

const here = dirname(fileURLToPath(import.meta.url));
const binary = join(here, "forge");

const child = spawn(binary, [], { stdio: "inherit", env: process.env });

child.on("error", (err) => {
  console.error(`forge: failed to start ${binary}: ${err.message}`);
  process.exit(1);
});
child.on("exit", (code, signal) => {
  if (signal) {
    console.error(`forge: terminated by ${signal}`);
    process.exit(1);
  }
  process.exit(code ?? 1);
});
