export const DEFAULT_ENV_NAME = "evandelacruz/farrier";
export const DEFAULT_REPO_URL = "https://github.com/evandelacruz/farrier";
export const DEFAULT_MODEL = process.env.CURSOR_MODEL ?? "composer-2.5";
export const DEFAULT_STARTING_REF = "main";

export function requireApiKey(): string {
  const key = process.env.CURSOR_API_KEY?.trim();
  if (!key) {
    throw new Error(
      "Set CURSOR_API_KEY (https://cursor.com/dashboard/api) before running this command.",
    );
  }
  return key;
}
