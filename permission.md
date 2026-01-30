# Permissions to Request from IT (SharePoint / Goodmem Sync)

There is **no separate “webhook” permission**. Microsoft Graph sends notifications to your URL; the app only needs the same **read** permissions to create the subscription and to fetch changed files when a notification arrives. If your app already has these for `main.py`, nothing extra is required for the webhook.

Ask your IT division to grant the following for the Azure AD app used by this project. The app is identified by the credentials in the project’s `.env` file:

| In `.env` | In Azure Portal |
|-----------|------------------|
| **SHAREPOINT_CLIENT_ID** | Application (client) ID |
| **SHAREPOINT_TENANT_ID** | Directory (tenant) ID |

IT should find the app whose **Application (client) ID** matches the value of **SHAREPOINT_CLIENT_ID** in `.env`.

---

## Azure AD (Microsoft Entra) – Application permissions

| Permission (Microsoft Graph) | Type        | What it’s for |
|----------------------------|-------------|----------------|
| **Files.Read.All**         | Application | Read **files/drive items** (content, metadata, download URLs). Required to create the webhook subscription and to fetch changed files when a notification arrives. |
| **Sites.Read.All**         | Application | Read **site/list metadata** (resolve site ID from URL, list drives for a site). Used to discover which site and drive to subscribe to from **SHAREPOINT_SITE_URL**. |

**Both** are needed for the webhook with the current code: **Files.Read.All** for the subscription and file reads; **Sites.Read.All** to turn your site URL into a site ID and drive ID. Without **Sites.Read.All**, `graph_listener.py` cannot call the Graph “sites” APIs and will fail when starting the server or creating the subscription.

**Where to set these (Azure Portal):**

1. Sign in to the **Azure Portal** (https://portal.azure.com).
2. Go to **Microsoft Entra ID** (or **Azure Active Directory**).
3. In the left menu, click **App registrations**.
4. Find and open the app whose **Application (client) ID** equals `aab64d5b-7fe3-492f-961c-07ed5c0df983`.
5. In the app’s left menu, click **API permissions**.
6. Click **Add a permission**.
7. Choose **Microsoft Graph**.
8. Choose **Application permissions** (not “Delegated permissions”).
9. In the search/list, find and check **Sites.Read.All**.
10. Click **Add permissions**.
11. Click **Grant admin consent for [your organization]** (top of the API permissions page). Confirm if prompted.
12. Confirm that both permissions show **Granted for [your organization]** in the “Status” column.

---

## SharePoint

- Ensure the app whose Client ID is **SHAREPOINT_CLIENT_ID** (in `.env`) is **allowed to access** the target SharePoint site. The site URL is **SHAREPOINT_SITE_URL** in `.env`. If conditional access or policies block application access to that site, IT may need to allow the app.
