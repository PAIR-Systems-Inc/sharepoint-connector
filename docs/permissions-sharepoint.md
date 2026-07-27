# Request for IT: Azure AD app for SharePoint → Goodmem sync

**Request:** Please create an Azure AD (Microsoft Entra ID) app registration with **Application permissions** and provide us with **client_id**, **client_secret**, and **tenant_id** (for our project's `.env` as AZURE_AD_CLIENT_ID, AZURE_AD_CLIENT_SECRET, AZURE_AD_TENANT_ID).

**Permissions to grant:** **Files.Read.All** and **Sites.Read.All** (Microsoft Graph, Application permissions). Both are required for our sync and webhook listener.

Instructions below follow [Airbyte: Microsoft SharePoint – Set up SharePoint application](https://docs.airbyte.com/integrations/sources/microsoft-sharepoint#step-1-set-up-sharepoint-application). We add **Sites.Read.All** in addition to Files.Read.All (step 13).

---

## Steps (for IT)

1. Login to Azure Portal  
   [https://portal.azure.com](https://portal.azure.com)

2. Click upper-left menu icon and select **Azure Active Directory** (or **Microsoft Entra ID**).

3. Select **App Registrations**.

4. Click **New registration**.

5. Register an application  
   - **Name:** (e.g. SharePoint Goodmem Sync)  
   - **Supported account types:** Accounts in this organizational directory only  
   - **Register** (button)

6. Record the **client_id** (Application (client) ID) and **tenant_id** (Directory (tenant) ID). **Provide these to us** (AZURE_AD_CLIENT_ID and AZURE_AD_TENANT_ID).

7. Select **Certificates & secrets**.

8. Provide **Description and Expires**  
   - **Description:** (e.g. SharePoint Goodmem Sync client secret)  
   - **Expires:** 1-year (or your policy)  
   - **Add**

9. Copy the **client secret value**; this will be the **client_secret**. **Provide this to us** (AZURE_AD_CLIENT_SECRET). The value is shown only once.

10. Select **API permissions**  
    - Click **Add a permission**

11. Select **Microsoft Graph**.

12. Select **Application permissions** (not Delegated permissions).

13. Select the following permissions:  
    - **Files** → **Files.Read.All**  
    - **Sites** → **Sites.Read.All**

14. Click **Add permissions**.

15. Click **Grant admin consent** (for your organization). Confirm if prompted.

---

## Optional (if your policies restrict app access by site)

If you restrict which apps can access which SharePoint sites, please allow this app (same Application (client) ID) to access the SharePoint site at **SHAREPOINT_SITE_URL** (we will provide the site URL). Some orgs have complex permission systems that require this step.

---

## How we verify

After we receive the credentials, we run `python test_graph_permissions.py` to confirm the app can authenticate, resolve the site, list drives, and list files. No webhook or server deployment is required for this check.
